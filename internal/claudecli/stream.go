package claudecli

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"log"
	"strings"
	"sync"
)

// rates is a model's price in USD per million tokens.
type rates struct{ in, out, cacheWrite, cacheRead float64 }

// Price table for the live spend estimate.
//
// IMPORTANT: this is NOT an invoice. The factory runs on a Claude
// subscription usage plan, not API billing. The dollar figures here are
// an API-equivalent cost ESTIMATE that stands in for usage-window
// consumption (max_cost_usd caps that stand-in, not real money).
//
// Source: Anthropic published pricing,
// https://platform.claude.com/docs/en/about-claude/pricing (checked
// 2026-11-17). Cache writes are the 5-minute rate (1.25x input); cache
// reads are the "cache hits" column. The estimate is used ONLY while a
// run streams (assistant events carry token counts, not cost); the CLI's
// final result total_cost_usd, whenever present, replaces it.
//
// Haiku 5.5 is tiered by prompt size (<=100K: $0.10/$0.50, >100K:
// $0.50/$2.50); agent runs routinely exceed 100K so the higher tier is
// used, erring towards stopping early.
var (
	ratesOpus55   = rates{in: 4, out: 20, cacheWrite: 5, cacheRead: 0.20}
	ratesOpus     = rates{in: 5, out: 25, cacheWrite: 6.25, cacheRead: 0.50}   // Opus 5, 4.8, 4.7, 4.6, 4.5
	ratesOpusOld  = rates{in: 15, out: 75, cacheWrite: 18.75, cacheRead: 1.50} // Opus 4, 4.1
	ratesSonnet55 = rates{in: 2, out: 10, cacheWrite: 2.5, cacheRead: 0.10}
	ratesSonnet5  = rates{in: 2, out: 10, cacheWrite: 2.5, cacheRead: 0.20}
	ratesSonnet   = rates{in: 3, out: 15, cacheWrite: 3.75, cacheRead: 0.30} // Sonnet 4.x
	ratesHaiku55  = rates{in: 0.5, out: 2.5, cacheWrite: 0.625, cacheRead: 0.05}
	ratesHaiku    = rates{in: 1, out: 5, cacheWrite: 1.25, cacheRead: 0.10} // Haiku 4.5
)

// familyRates maps a model-id substring to its rates; first match wins,
// so more specific ids come before their families. Ids the factory uses:
// claude-opus-5-5 (planner default, internal/models), claude-opus-4-6
// (documented worker-tier override); the rest cover other claude-*
// overrides and bare aliases (opus/sonnet/haiku map to the family's
// higher-priced current rate, never lower).
var familyRates = []struct {
	match string
	r     rates
}{
	{"opus-5-5", ratesOpus55},
	{"opus-4-1", ratesOpusOld},
	{"opus-4-2025", ratesOpusOld}, // claude-opus-4-<date>
	{"opus", ratesOpus},
	{"sonnet-5-5", ratesSonnet55},
	{"sonnet-5", ratesSonnet5},
	{"sonnet", ratesSonnet},
	{"haiku-5", ratesHaiku55},
	{"haiku", ratesHaiku},
}

// lookupRates returns the rates for a claude model id (case-insensitive,
// provider prefixes tolerated) and whether the family is known.
func lookupRates(model string) (rates, bool) {
	m := strings.ToLower(model)
	for _, f := range familyRates {
		if strings.Contains(m, f.match) {
			return f.r, true
		}
	}
	return rates{}, false
}

// defaultUnknownRates is the last resort when neither the event's model
// nor the tier model is a known claude family (e.g. a non-claude id): a
// mid-priced Sonnet 4.x rate, never the most expensive one.
var defaultUnknownRates = ratesSonnet

var warnedModels sync.Map

func warnOnce(model, fallback string) {
	if _, dup := warnedModels.LoadOrStore(model, true); !dup {
		log.Printf("claudecli: unknown model %q in price table; estimating with %q rates", model, fallback)
	}
}

// ratesFor resolves rates for model; unknown models fall back to the
// tier's model (fallbackModel, the harness's configured model), and if
// that is unknown too, to defaultUnknownRates. Falling back logs a warning.
func ratesFor(model, fallbackModel string) rates {
	if r, ok := lookupRates(model); ok {
		return r
	}
	if r, ok := lookupRates(fallbackModel); ok {
		warnOnce(model, fallbackModel)
		return r
	}
	warnOnce(model, "default-sonnet")
	return defaultUnknownRates
}

// EstimateCostUSD prices one usage block for model (API-equivalent
// estimate, not an invoice - see the price table notes). fallbackModel is
// used when model is not a known family.
func EstimateCostUSD(model, fallbackModel string, u Usage) float64 {
	r := ratesFor(model, fallbackModel)
	return (float64(u.Input)*r.in + float64(u.Output)*r.out +
		float64(u.CacheCreate)*r.cacheWrite + float64(u.CacheRead)*r.cacheRead) / 1e6
}

// Stream incrementally reads `claude --output-format stream-json` output
// (one JSON object per line) while the run is in flight. It accumulates
// spend (an API-equivalent estimate standing in for subscription usage,
// not real money) and tracks back-to-back identical tool calls. It is safe for
// concurrent use; partial trailing lines are buffered across Feed calls.
type Stream struct {
	mu      sync.Mutex
	partial []byte
	seenMsg map[string]bool
	cost    float64
	final   bool // cost is the authoritative result total
	hasCost bool

	fallbackModel string            // tier model, used for unknown-model pricing
	pending       map[string]string // tool_use id -> name\x00compact-input
	lastSig       string            // name\x00input\x00result-hash of the last completed call
	repeats       int
}

// NewStream returns an empty Stream. fallbackModel is the tier's model,
// whose rates price assistant events naming an unknown model.
func NewStream(fallbackModel string) *Stream {
	return &Stream{seenMsg: map[string]bool{}, pending: map[string]string{}, fallbackModel: fallbackModel}
}

type streamEvent struct {
	Type         string  `json:"type"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	Message      *struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage *struct {
			Input       int `json:"input_tokens"`
			Output      int `json:"output_tokens"`
			CacheCreate int `json:"cache_creation_input_tokens"`
			CacheRead   int `json:"cache_read_input_tokens"`
		} `json:"usage"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// Feed consumes a chunk of child stdout.
func (s *Stream) Feed(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimSpace(s.partial[:i])
		s.partial = s.partial[i+1:]
		if len(line) > 0 && line[0] == '{' {
			s.event(line)
		}
	}
	// A pathological unterminated line must not grow without bound.
	if len(s.partial) > 8<<20 {
		s.partial = nil
	}
}

func (s *Stream) event(line []byte) {
	var e streamEvent
	if json.Unmarshal(line, &e) != nil {
		return
	}
	switch e.Type {
	case "result":
		if e.TotalCostUSD > 0 {
			s.cost, s.final, s.hasCost = e.TotalCostUSD, true, true
		}
	case "assistant":
		if e.Message == nil {
			return
		}
		// One API message is streamed as several events (one per content
		// block) repeating the same usage: price each message id once.
		if u := e.Message.Usage; u != nil && !s.final && !s.seenMsg[e.Message.ID] {
			if e.Message.ID != "" {
				s.seenMsg[e.Message.ID] = true
			}
			s.cost += EstimateCostUSD(e.Message.Model, s.fallbackModel, Usage{
				Input: u.Input, Output: u.Output, CacheCreate: u.CacheCreate, CacheRead: u.CacheRead,
			})
			s.hasCost = true
		}
		s.blocks(e.Message.Content)
	case "user":
		if e.Message != nil {
			s.blocks(e.Message.Content)
		}
	}
}

func (s *Stream) blocks(raw json.RawMessage) {
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return
	}
	for _, b := range blocks {
		switch b.Type {
		case "tool_use":
			s.toolUse(b)
		case "tool_result":
			s.toolResult(b)
		}
	}
}

func (s *Stream) toolUse(b contentBlock) {
	var canon bytes.Buffer
	if json.Compact(&canon, b.Input) != nil {
		canon.Reset()
		canon.Write(b.Input)
	}
	s.pending[b.ID] = b.Name + "\x00" + canon.String()
}

// toolResult completes a call. A call counts as a repeat only when its
// tool name, compacted input AND result are all identical to the previous
// completed call; so polling `make ci` or `git status` while the output
// changes is not a loop.
func (s *Stream) toolResult(b contentBlock) {
	key, ok := s.pending[b.ToolUseID]
	if !ok {
		return
	}
	delete(s.pending, b.ToolUseID)
	var canon bytes.Buffer
	if json.Compact(&canon, b.Content) != nil {
		canon.Reset()
		canon.Write(b.Content)
	}
	sum := sha256.Sum256(canon.Bytes())
	sig := key + "\x00" + string(sum[:])
	if sig == s.lastSig {
		s.repeats++
	} else {
		s.lastSig, s.repeats = sig, 1
	}
}

// Cost returns the spend so far and whether any usage data was seen. It is
// an estimate until the result event arrives, then the CLI's own total.
func (s *Stream) Cost() (usd float64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cost, s.hasCost
}

// Repeats returns how many times in a row the most recent completed tool
// call (same name, identical input, identical result) has occurred; 0
// before any completed tool call.
func (s *Stream) Repeats() (n int, tool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, _, _ := strings.Cut(s.lastSig, "\x00")
	return s.repeats, name
}
