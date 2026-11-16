package claudecli

import (
	"bytes"
	"encoding/json"
	"strings"
	"sync"
)

// rates is a model family's price in USD per million tokens.
type rates struct{ in, out float64 }

// Estimated list prices per family, used ONLY for the live spend cap
// while a run is streaming: stream-json assistant events carry token
// counts but no cost (the authoritative total_cost_usd arrives on the
// final result event, which overrides the estimate). Cache reads bill at
// 0.1x input, cache writes at 1.25x input. Unknown models are priced as
// opus so the cap errs on the side of stopping early.
var (
	ratesOpus   = rates{in: 15, out: 75}
	ratesSonnet = rates{in: 3, out: 15}
	ratesHaiku  = rates{in: 1, out: 5}
)

func ratesFor(model string) rates {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "sonnet"):
		return ratesSonnet
	case strings.Contains(m, "haiku"):
		return ratesHaiku
	default:
		return ratesOpus
	}
}

// EstimateCostUSD prices one usage block for model.
func EstimateCostUSD(model string, u Usage) float64 {
	r := ratesFor(model)
	return (float64(u.Input)*r.in + float64(u.Output)*r.out +
		float64(u.CacheCreate)*r.in*1.25 + float64(u.CacheRead)*r.in*0.1) / 1e6
}

// Stream incrementally reads `claude --output-format stream-json` output
// (one JSON object per line) while the run is in flight. It accumulates
// spend and tracks back-to-back identical tool calls. It is safe for
// concurrent use; partial trailing lines are buffered across Feed calls.
type Stream struct {
	mu      sync.Mutex
	partial []byte
	seenMsg map[string]bool
	cost    float64
	final   bool // cost is the authoritative result total
	hasCost bool

	lastTool string
	repeats  int
}

// NewStream returns an empty Stream.
func NewStream() *Stream { return &Stream{seenMsg: map[string]bool{}} }

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
	Type  string          `json:"type"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
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
			s.cost += EstimateCostUSD(e.Message.Model, Usage{
				Input: u.Input, Output: u.Output, CacheCreate: u.CacheCreate, CacheRead: u.CacheRead,
			})
			s.hasCost = true
		}
		var blocks []contentBlock
		if json.Unmarshal(e.Message.Content, &blocks) == nil {
			for _, b := range blocks {
				if b.Type == "tool_use" {
					s.tool(b)
				}
			}
		}
	}
}

func (s *Stream) tool(b contentBlock) {
	var canon bytes.Buffer
	if json.Compact(&canon, b.Input) != nil {
		canon.Reset()
		canon.Write(b.Input)
	}
	key := b.Name + "\x00" + canon.String()
	if key == s.lastTool {
		s.repeats++
	} else {
		s.lastTool, s.repeats = key, 1
	}
}

// Cost returns the spend so far and whether any usage data was seen. It is
// an estimate until the result event arrives, then the CLI's own total.
func (s *Stream) Cost() (usd float64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cost, s.hasCost
}

// Repeats returns how many times in a row the most recent tool call (same
// name, identical input) has been made; 0 before any tool call.
func (s *Stream) Repeats() (n int, tool string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	name, _, _ := strings.Cut(s.lastTool, "\x00")
	return s.repeats, name
}
