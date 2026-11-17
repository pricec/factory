package summarize

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeOllama serves /api/chat with handler and records the last request.
func fakeOllama(t *testing.T, handler func(w http.ResponseWriter, req chatRequest)) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		handler(w, req)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func reply(content, doneReason string) func(http.ResponseWriter, chatRequest) {
	return func(w http.ResponseWriter, _ chatRequest) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message":     map[string]any{"role": "assistant", "content": content, "thinking": "private"},
			"done_reason": doneReason,
			"eval_count":  12,
		})
	}
}

var longInput = strings.Repeat("job 42 built foo/bar.go; PR #17 merged; exit 0.\n", 200) // ~9.8KB

func TestSummarize(t *testing.T) {
	good := "- Job 42 built foo/bar.go\n- PR #17 merged, exit 0"
	tests := []struct {
		name      string
		handler   func(http.ResponseWriter, chatRequest)
		input     string
		disabled  bool
		timeout   time.Duration
		wantErr   error // sentinel, or nil
		wantRej   bool  // a *RejectedError
		wantAnyEr bool  // any other error
		wantCalls int32
	}{
		{name: "success", handler: reply(good, "stop"), input: longInput, wantCalls: 1},
		{name: "disabled", handler: reply(good, "stop"), input: longInput, disabled: true, wantErr: ErrDisabled},
		{name: "below threshold", handler: reply(good, "stop"), input: "short", wantErr: ErrBelowThreshold},
		{name: "empty output", handler: reply("   \n", "stop"), input: longInput, wantRej: true, wantCalls: 1},
		{name: "only a think block", handler: reply("<think>hmm</think>", "stop"), input: longInput, wantRej: true, wantCalls: 1},
		{name: "reasoning leak lets", handler: reply("Let's summarise the job. "+good, "stop"), input: longInput, wantRej: true, wantCalls: 1},
		{name: "reasoning leak we need", handler: reply("We need to produce a summary.", "stop"), input: longInput, wantRej: true, wantCalls: 1},
		{name: "reasoning leak harmony", handler: reply("analysisThe user wants...", "stop"), input: longInput, wantRej: true, wantCalls: 1},
		{name: "garbage longer than input", handler: reply(longInput+longInput, "stop"), input: longInput, wantRej: true, wantCalls: 1},
		{name: "hit token limit", handler: reply(good, "length"), input: longInput, wantRej: true, wantCalls: 1},
		{
			name: "http error", input: longInput, wantAnyEr: true, wantCalls: 1,
			handler: func(w http.ResponseWriter, _ chatRequest) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"model not found"}`))
			},
		},
		{
			name: "non-json garbage", input: longInput, wantAnyEr: true, wantCalls: 1,
			handler: func(w http.ResponseWriter, _ chatRequest) { _, _ = w.Write([]byte("<html>proxy error</html>")) },
		},
		{
			name: "timeout", input: longInput, timeout: 50 * time.Millisecond, wantAnyEr: true, wantCalls: 1,
			handler: func(w http.ResponseWriter, _ chatRequest) {
				time.Sleep(500 * time.Millisecond)
				reply(good, "stop")(w, chatRequest{})
			},
		},
		{name: "think block stripped", handler: reply("<think>x</think>\n"+good, "stop"), input: longInput, wantCalls: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := fakeOllama(t, tc.handler)
			cfg := Config{URL: srv.URL, MinBytes: 1024, Timeout: tc.timeout}
			if tc.disabled {
				cfg.URL = ""
			}
			c := New(cfg)
			s, err := c.Summarize(context.Background(), tc.input)
			var rej *RejectedError
			switch {
			case tc.wantErr != nil:
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
			case tc.wantRej:
				if !errors.As(err, &rej) {
					t.Fatalf("err = %v, want RejectedError", err)
				}
			case tc.wantAnyEr:
				if err == nil || errors.As(err, &rej) {
					t.Fatalf("err = %v, want a transport error", err)
				}
			default:
				if err != nil {
					t.Fatalf("unexpected err: %v", err)
				}
				if s.Text != good || s.Model != DefaultModel || s.InputBytes != len(tc.input) {
					t.Fatalf("summary = %+v", s)
				}
			}
			if got := calls.Load(); got != tc.wantCalls {
				t.Fatalf("ollama calls = %d, want %d", got, tc.wantCalls)
			}
			// Condense always falls back to the original on failure.
			if err != nil {
				if out, ok := c.Condense(context.Background(), tc.input); ok || out != tc.input {
					t.Fatalf("Condense fallback = (%q..., %v), want original", firstWords(out, 4), ok)
				}
			}
		})
	}
}

func TestRequestShape(t *testing.T) {
	tests := []struct {
		model, think string
		want         any
	}{
		{"qwen3:8b", "", false},
		{"gpt-oss:20b", "", "low"},
		{"qwen3:8b", "true", true},
		{"gpt-oss:20b", "high", "high"},
	}
	for _, tc := range tests {
		t.Run(tc.model+"/"+tc.think, func(t *testing.T) {
			var got chatRequest
			srv, _ := fakeOllama(t, func(w http.ResponseWriter, req chatRequest) {
				got = req
				reply("- ok", "stop")(w, req)
			})
			if _, err := New(Config{URL: srv.URL, Model: tc.model, Think: tc.think, MinBytes: 1}).Summarize(context.Background(), longInput); err != nil {
				t.Fatal(err)
			}
			if got.Stream || got.Model != tc.model || got.Think != tc.want {
				t.Fatalf("request = model %q stream %v think %#v", got.Model, got.Stream, got.Think)
			}
			if got.Options["temperature"] != float64(0) || got.Options["num_ctx"] != float64(DefaultNumCtx) {
				t.Fatalf("options = %v", got.Options)
			}
			if len(got.Messages) != 2 || got.Messages[0].Content != SystemPrompt || !strings.Contains(got.Messages[1].Content, longInput) {
				t.Fatalf("messages wrong")
			}
		})
	}
}

func TestClipTruncatesAndSays(t *testing.T) {
	var got chatRequest
	srv, _ := fakeOllama(t, func(w http.ResponseWriter, req chatRequest) {
		got = req
		reply("- truncated input; job 42", "stop")(w, req)
	})
	big := strings.Repeat("é", 3000) // 6000 bytes, multibyte
	s, err := New(Config{URL: srv.URL, MinBytes: 1, MaxInputBytes: 1001}).Summarize(context.Background(), big)
	if err != nil {
		t.Fatal(err)
	}
	if !s.Truncated || !strings.Contains(got.Messages[1].Content, "TRUNCATED") || !strings.Contains(got.Messages[1].Content, "bytes omitted") {
		t.Fatalf("truncation not signalled: %+v", s)
	}
	if !strings.HasPrefix(got.Messages[1].Content, "Job output (6000 bytes") {
		t.Fatalf("header = %q", got.Messages[1].Content[:40])
	}
}

func TestLoadConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cfg, warns := LoadConfig(env(nil))
	if cfg.Enabled() || len(warns) != 0 || cfg.Model != DefaultModel || cfg.MinBytes != DefaultMinBytes {
		t.Fatalf("default config = %+v %v", cfg, warns)
	}
	cfg, warns = LoadConfig(env(map[string]string{
		"FACTORY_SUMMARIZER_URL":       "http://openclaw:11434/",
		"FACTORY_SUMMARIZER_MODEL":     "gpt-oss:20b",
		"FACTORY_SUMMARIZER_MIN_BYTES": "8000",
		"FACTORY_SUMMARIZER_TIMEOUT":   "45s",
		"FACTORY_SUMMARIZER_READ_WAIT": "0s",
	}))
	if !cfg.Enabled() || cfg.URL != "http://openclaw:11434" || cfg.Model != "gpt-oss:20b" || cfg.MinBytes != 8000 ||
		cfg.Timeout != 45*time.Second || cfg.ReadWait != 0 || len(warns) != 0 {
		t.Fatalf("config = %+v %v", cfg, warns)
	}
	cfg, warns = LoadConfig(env(map[string]string{
		"FACTORY_SUMMARIZER_URL":       "http://x",
		"FACTORY_SUMMARIZER_MIN_BYTES": "lots",
		"FACTORY_SUMMARIZER_TIMEOUT":   "soon",
		"FACTORY_SUMMARIZER_THINK":     "maybe",
	}))
	if len(warns) != 3 || cfg.MinBytes != DefaultMinBytes || cfg.Timeout != DefaultTimeout || cfg.Think != "" {
		t.Fatalf("bad values not defaulted: %+v %v", cfg, warns)
	}
}

func TestResultText(t *testing.T) {
	tests := []struct{ in, want string }{
		{``, ``},
		{`null`, ``},
		{`"line1\nline2"`, "line1\nline2"},
		{`{"a":1}`, `{"a":1}`},
	}
	for _, tc := range tests {
		if got := ResultText(json.RawMessage(tc.in)); got != tc.want {
			t.Errorf("ResultText(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
