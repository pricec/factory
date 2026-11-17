// Package summarize is the local-model summarisation pre-pass (spec
// §6.13): long job output is condensed by a no-tools model served by
// Ollama (/api/chat) so the planner can read a digest instead of the
// verbatim text. It is advisory only - the caller always keeps the
// original, and every failure (disabled, below threshold, Ollama down,
// slow, or returning output that fails the sanity check) yields an error
// so the caller falls back to the original unchanged.
package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Defaults (spec §6.13).
const (
	DefaultModel         = "qwen3:8b"
	DefaultMinBytes      = 6144
	DefaultTimeout       = 90 * time.Second
	DefaultReadWait      = 60 * time.Second
	DefaultNumCtx        = 32768
	DefaultNumPredict    = 2048
	DefaultMaxInputBytes = 80 * 1024
	// DefaultKeepAlive is sent with every request so the model is
	// unloaded soon after the last digest; Ollama's server default may
	// pin it on the GPUs for much longer.
	DefaultKeepAlive = "10m"
	// DefaultMaxRatio: a digest must be at most this fraction of the
	// input's bytes; anything larger saves too little to be worth the risk.
	DefaultMaxRatio = 0.8
)

// SystemPrompt is the fixed instruction sent with every request.
const SystemPrompt = `You condense the output of an automated software job for an operator who will decide what to do next.
Rules:
- Keep EVERY concrete fact: numbers, scores, sizes, timings, counts, IDs (job ids, commit SHAs, PR and issue numbers), file paths, URLs, commands, model names, version strings, error messages and exit codes. Copy them exactly.
- Keep the outcome (done, failed, partial), any recommendation, open questions, risks and next steps.
- Never invent anything that is not in the input. Do not add advice of your own.
- Drop repetition, narrative filler and pleasantries. Tables may be condensed to compact lists but keep their numbers.
- Be terse: short bullet points, no full sentences where a fragment will do. Stay within the word budget given with the input.
- Output only the summary in plain Markdown. No preamble, no reasoning, no closing remarks.`

// Errors returned by Summarize; every one means "use the original".
var (
	ErrDisabled       = errors.New("summarize: disabled (FACTORY_SUMMARIZER_URL unset)")
	ErrBelowThreshold = errors.New("summarize: input below threshold")
)

// RejectedError is a model reply that failed the sanity check.
type RejectedError struct{ Reason string }

func (e *RejectedError) Error() string { return "summarize: output rejected: " + e.Reason }

// Config configures the summariser. The zero value (empty URL) is disabled.
type Config struct {
	// URL is the Ollama base URL, e.g. http://openclaw:11434. Empty
	// disables summarisation entirely.
	URL string
	// Model is the Ollama model tag.
	Model string
	// MinBytes: inputs shorter than this are never summarised.
	MinBytes int
	// Timeout bounds one request end to end (model load included).
	Timeout time.Duration
	// ReadWait bounds how long read_result waits for an in-flight digest.
	ReadWait time.Duration
	// Think is Ollama's think parameter: "false", "true", "low",
	// "medium" or "high". Empty = auto: "low" for gpt-oss (which cannot
	// turn thinking off), otherwise "false". Thinking text is returned by
	// Ollama in a separate field and is never used.
	Think string
	// KeepAlive is Ollama's keep_alive for each request (a Go-style
	// duration such as "10m", or "0"); empty -> DefaultKeepAlive.
	KeepAlive string
	// NumCtx, NumPredict, MaxInputBytes, MaxRatio: zero -> defaults.
	NumCtx        int
	NumPredict    int
	MaxInputBytes int
	MaxRatio      float64
	// HTTPClient: nil -> a client without its own timeout (Timeout
	// applies via the request context).
	HTTPClient *http.Client
}

// Enabled reports whether a URL is configured.
func (c Config) Enabled() bool { return c.URL != "" }

// LoadConfig reads the FACTORY_SUMMARIZER_* env (spec §6.13). Bad values
// fall back to the default with a warning; never fatal.
func LoadConfig(getenv func(string) string) (Config, []string) {
	var warns []string
	cfg := Config{
		URL:   strings.TrimRight(strings.TrimSpace(getenv("FACTORY_SUMMARIZER_URL")), "/"),
		Model: strings.TrimSpace(getenv("FACTORY_SUMMARIZER_MODEL")),
		Think: strings.TrimSpace(getenv("FACTORY_SUMMARIZER_THINK")),
	}
	if v := strings.TrimSpace(getenv("FACTORY_SUMMARIZER_KEEP_ALIVE")); v != "" {
		if _, err := time.ParseDuration(v); err != nil && v != "0" {
			warns = append(warns, fmt.Sprintf("FACTORY_SUMMARIZER_KEEP_ALIVE=%q invalid; using %s", v, DefaultKeepAlive))
		} else {
			cfg.KeepAlive = v
		}
	}
	if v := strings.TrimSpace(getenv("FACTORY_SUMMARIZER_MIN_BYTES")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			warns = append(warns, fmt.Sprintf("FACTORY_SUMMARIZER_MIN_BYTES=%q invalid; using %d", v, DefaultMinBytes))
		} else {
			cfg.MinBytes = n
		}
	}
	for _, d := range []struct {
		env string
		dst *time.Duration
		def time.Duration
	}{
		{"FACTORY_SUMMARIZER_TIMEOUT", &cfg.Timeout, DefaultTimeout},
		{"FACTORY_SUMMARIZER_READ_WAIT", &cfg.ReadWait, DefaultReadWait},
	} {
		v := strings.TrimSpace(getenv(d.env))
		if v == "" {
			continue
		}
		dur, err := time.ParseDuration(v)
		if err != nil || dur < 0 {
			warns = append(warns, fmt.Sprintf("%s=%q invalid; using %s", d.env, v, d.def))
			continue
		}
		*d.dst = dur
	}
	switch strings.ToLower(cfg.Think) {
	case "", "false", "true", "low", "medium", "high":
		cfg.Think = strings.ToLower(cfg.Think)
	default:
		warns = append(warns, fmt.Sprintf("FACTORY_SUMMARIZER_THINK=%q invalid; using auto", cfg.Think))
		cfg.Think = ""
	}
	return cfg.withDefaults(), warns
}

func (c Config) withDefaults() Config {
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.MinBytes <= 0 {
		c.MinBytes = DefaultMinBytes
	}
	if c.Timeout <= 0 {
		c.Timeout = DefaultTimeout
	}
	if c.ReadWait < 0 {
		c.ReadWait = 0
	}
	if c.KeepAlive == "" {
		c.KeepAlive = DefaultKeepAlive
	}
	if c.NumCtx <= 0 {
		c.NumCtx = DefaultNumCtx
	}
	if c.NumPredict <= 0 {
		c.NumPredict = DefaultNumPredict
	}
	if c.MaxInputBytes <= 0 {
		c.MaxInputBytes = DefaultMaxInputBytes
	}
	if c.MaxRatio <= 0 {
		c.MaxRatio = DefaultMaxRatio
	}
	if c.HTTPClient == nil {
		c.HTTPClient = &http.Client{}
	}
	return c
}

// think resolves the Ollama think value for the configured model.
func (c Config) think() any {
	switch c.Think {
	case "true":
		return true
	case "false":
		return false
	case "low", "medium", "high":
		return c.Think
	}
	if strings.HasPrefix(c.Model, "gpt-oss") {
		return "low"
	}
	return false
}

// Client calls Ollama.
type Client struct{ cfg Config }

// New returns a client for cfg (defaults applied).
func New(cfg Config) *Client { return &Client{cfg: cfg.withDefaults()} }

// Config returns the effective configuration.
func (c *Client) Config() Config { return c.cfg }

// Raw is one model reply plus Ollama's timing counters.
type Raw struct {
	Content    string        // message.content as returned (pre-check)
	Thinking   int           // bytes of message.thinking (never used)
	DoneReason string        // "stop", "length", ...
	Truncated  bool          // the input was cut to MaxInputBytes
	InputBytes int           // bytes of the original input
	Duration   time.Duration // wall clock of the HTTP call
	// Ollama counters: load_duration distinguishes a cold call.
	LoadDuration    time.Duration
	PromptEvalCount int
	EvalCount       int
}

// Summary is a digest that passed the sanity check.
type Summary struct {
	Text       string
	Model      string
	InputBytes int
	Truncated  bool
	Duration   time.Duration
}

// Summarize condenses text, or returns an error - in which case the
// caller must use text unchanged. It never returns a digest that failed
// Check.
func (c *Client) Summarize(ctx context.Context, text string) (Summary, error) {
	if c == nil || !c.cfg.Enabled() {
		return Summary{}, ErrDisabled
	}
	if len(text) < c.cfg.MinBytes {
		return Summary{}, ErrBelowThreshold
	}
	raw, err := c.Call(ctx, text)
	if err != nil {
		return Summary{}, err
	}
	out, err := Check(text, raw, c.cfg.MaxRatio)
	if err != nil {
		return Summary{}, err
	}
	return Summary{Text: out, Model: c.cfg.Model, InputBytes: len(text), Truncated: raw.Truncated, Duration: raw.Duration}, nil
}

// Condense is Summarize with the fallback built in: the digest on
// success, the original text otherwise.
func (c *Client) Condense(ctx context.Context, text string) (string, bool) {
	s, err := c.Summarize(ctx, text)
	if err != nil {
		return text, false
	}
	return s.Text, true
}

type chatRequest struct {
	Model     string         `json:"model"`
	Messages  []chatMessage  `json:"messages"`
	Stream    bool           `json:"stream"`
	Think     any            `json:"think"`
	KeepAlive string         `json:"keep_alive,omitempty"`
	Options   map[string]any `json:"options"`
}

type chatMessage struct {
	Role     string `json:"role"`
	Content  string `json:"content"`
	Thinking string `json:"thinking,omitempty"`
}

type chatResponse struct {
	Message         chatMessage `json:"message"`
	DoneReason      string      `json:"done_reason"`
	Error           string      `json:"error"`
	LoadDuration    int64       `json:"load_duration"`
	PromptEvalCount int         `json:"prompt_eval_count"`
	EvalCount       int         `json:"eval_count"`
}

// Call sends one /api/chat request (stream=false, temperature 0) bounded
// by cfg.Timeout and returns the unchecked reply. It does not apply the
// threshold or the sanity check (summarize-eval uses it to see what the
// model actually said).
func (c *Client) Call(ctx context.Context, text string) (Raw, error) {
	if c == nil || !c.cfg.Enabled() {
		return Raw{}, ErrDisabled
	}
	input, truncated := clip(text, c.cfg.MaxInputBytes)
	note := ""
	if truncated {
		// Only a really clipped input carries the instruction: a standing
		// "say so if truncated" rule made qwen3:8b claim truncation on 6
		// of 20 complete inputs in the pilot.
		note = fmt.Sprintf(", TRUNCATED to %d bytes - the middle is omitted; start the summary with \"(input truncated)\"", len(input))
	}
	body, err := json.Marshal(chatRequest{
		Model: c.cfg.Model,
		Messages: []chatMessage{
			{Role: "system", Content: SystemPrompt},
			{Role: "user", Content: fmt.Sprintf("Job output (%d bytes%s). Word budget: at most %d words.\n\n%s", len(text), note, wordBudget(len(text)), input)},
		},
		Stream:    false,
		Think:     c.cfg.think(),
		KeepAlive: c.cfg.KeepAlive,
		Options: map[string]any{
			"temperature": 0,
			"num_ctx":     c.cfg.NumCtx,
			"num_predict": c.cfg.NumPredict,
		},
	})
	if err != nil {
		return Raw{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return Raw{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return Raw{}, fmt.Errorf("summarize: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	dur := time.Since(start)
	if err != nil {
		return Raw{}, fmt.Errorf("summarize: read: %w", err)
	}
	var cr chatResponse
	if jerr := json.Unmarshal(data, &cr); jerr != nil {
		return Raw{}, fmt.Errorf("summarize: HTTP %d: undecodable reply: %w", resp.StatusCode, jerr)
	}
	if resp.StatusCode != http.StatusOK || cr.Error != "" {
		return Raw{}, fmt.Errorf("summarize: HTTP %d: %s", resp.StatusCode, cr.Error)
	}
	return Raw{
		Content:         cr.Message.Content,
		Thinking:        len(cr.Message.Thinking),
		DoneReason:      cr.DoneReason,
		Truncated:       truncated,
		InputBytes:      len(text),
		Duration:        dur,
		LoadDuration:    time.Duration(cr.LoadDuration),
		PromptEvalCount: cr.PromptEvalCount,
		EvalCount:       cr.EvalCount,
	}, nil
}

// wordBudget is the summary length asked of the model: about a quarter
// of the input's words (~6 bytes per word), never under 120.
func wordBudget(inputBytes int) int {
	if w := inputBytes / 24; w > 120 {
		return w
	}
	return 120
}

// Unload asks Ollama to drop the model from memory (keep_alive 0).
func (c *Client) Unload(ctx context.Context) error {
	if c == nil || !c.cfg.Enabled() {
		return ErrDisabled
	}
	body, _ := json.Marshal(map[string]any{"model": c.cfg.Model, "keep_alive": 0})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.URL+"/api/generate", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unload %s: HTTP %d", c.cfg.Model, resp.StatusCode)
	}
	return nil
}

var thinkBlock = regexp.MustCompile(`(?s)<think>.*?</think>`)

// leakPrefixes open model reasoning rather than a summary.
var leakPrefixes = []string{
	"let's", "let us", "let me", "we need", "we have to", "we should", "we must",
	"i need", "i should", "i will ", "i'll ", "i must", "okay", "ok,", "ok so", "alright",
	"hmm", "so,", "so we", "the user", "user wants", "first, i", "<think", "thinking",
	"analysis", "we are given", "we've been", "the task is",
}

// Check is the sanity check (spec §6.13) applied to every reply: strips
// any inline <think> block, then rejects empty output, a reply cut off by
// the token limit, an obvious reasoning leak at the start, and output
// that is not meaningfully shorter than the input (> maxRatio of it). It
// returns the cleaned digest.
func Check(input string, raw Raw, maxRatio float64) (string, error) {
	if maxRatio <= 0 {
		maxRatio = DefaultMaxRatio
	}
	out := strings.TrimSpace(thinkBlock.ReplaceAllString(raw.Content, ""))
	if strings.Contains(out, "<think>") || strings.Contains(out, "</think>") {
		return "", &RejectedError{"unbalanced think tag"}
	}
	switch {
	case out == "":
		return "", &RejectedError{"empty output"}
	case !utf8.ValidString(out):
		return "", &RejectedError{"invalid UTF-8"}
	case raw.DoneReason == "length":
		return "", &RejectedError{"hit the output token limit (truncated)"}
	case float64(len(out)) > maxRatio*float64(len(input)):
		return "", &RejectedError{fmt.Sprintf("not shorter: %d bytes for %d input (max ratio %.2f)", len(out), len(input), maxRatio)}
	}
	head := strings.ToLower(strings.TrimLeft(out, "#*_> \t\n"))
	for _, p := range leakPrefixes {
		if strings.HasPrefix(head, p) {
			return "", &RejectedError{fmt.Sprintf("reasoning leak (starts %q)", firstWords(out, 6))}
		}
	}
	return out, nil
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

// clip cuts s to at most max bytes, keeping the head (2/3) and tail (1/3)
// at UTF-8 boundaries with an omission marker between them.
func clip(s string, max int) (string, bool) {
	if len(s) <= max {
		return s, false
	}
	headN := max * 2 / 3
	tailN := max - headN
	for headN > 0 && !utf8.RuneStart(s[headN]) {
		headN--
	}
	start := len(s) - tailN
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[:headN] + fmt.Sprintf("\n\n[... %d bytes omitted ...]\n\n", start-headN) + s[start:], true
}

// ResultText renders a JobResult.result for summarisation: a JSON string
// is unquoted; any other JSON value is returned as its (compact) text;
// null/absent is empty.
func ResultText(result json.RawMessage) string {
	t := bytes.TrimSpace(result)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return ""
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(t, &s); err == nil {
			return s
		}
	}
	return string(t)
}
