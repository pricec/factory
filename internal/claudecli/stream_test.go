package claudecli

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

func asst(id, model string, in, out int, tools ...string) string {
	blocks := ""
	for _, t := range tools {
		blocks += t
	}
	if blocks == "" {
		blocks = `{"type":"text","text":"x"}`
	}
	return fmt.Sprintf(`{"type":"assistant","message":{"id":%q,"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d},"content":[%s]}}`+"\n", id, model, in, out, blocks)
}

func use(id, name, input string) string {
	return fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":%s}`, id, name, input)
}

func result(id, content string) string {
	return fmt.Sprintf(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`+"\n", id, content)
}

func TestStreamCost(t *testing.T) {
	t.Parallel()
	s := NewStream("claude-sonnet-5-5")
	if _, ok := s.Cost(); ok {
		t.Fatal("cost reported before any usage")
	}
	l := asst("m1", "claude-sonnet-5-5", 1000000, 0)
	s.Feed([]byte(l[:20])) // split mid-line
	s.Feed([]byte(l[20:]))
	s.Feed([]byte(l)) // duplicate message id priced once
	if c, ok := s.Cost(); !ok || math.Abs(c-2) > 1e-9 {
		t.Errorf("cost = %v,%v want 2 (1M sonnet-5-5 input tokens, once)", c, ok)
	}
	// The result event's total is authoritative and replaces the estimate.
	s.Feed([]byte(`{"type":"result","total_cost_usd":0.5}` + "\n"))
	s.Feed([]byte(asst("m2", "claude-sonnet-5-5", 1000000, 0)))
	if c, _ := s.Cost(); c != 0.5 {
		t.Errorf("cost = %v, want result total 0.5", c)
	}
}

func TestEstimateCostPriceTable(t *testing.T) {
	t.Parallel()
	u := Usage{Input: 1e6, Output: 1e6, CacheCreate: 1e6, CacheRead: 1e6}
	for _, tc := range []struct {
		model string
		want  float64 // in + out + cache write + cache read, $/MTok
	}{
		{"claude-opus-5-5", 4 + 20 + 5 + 0.20},
		{"claude-opus-4-6", 5 + 25 + 6.25 + 0.50},
		{"anthropic/claude-opus-4-8", 5 + 25 + 6.25 + 0.50},
		{"claude-opus-4-1", 15 + 75 + 18.75 + 1.50},
		{"claude-sonnet-5-5", 2 + 10 + 2.5 + 0.10},
		{"claude-sonnet-5", 2 + 10 + 2.5 + 0.20},
		{"claude-sonnet-4-6", 3 + 15 + 3.75 + 0.30},
		{"claude-haiku-4-5", 1 + 5 + 1.25 + 0.10},
		{"claude-haiku-5-5", 0.5 + 2.5 + 0.625 + 0.05},
		{"opus", 5 + 25 + 6.25 + 0.50},
	} {
		if got := EstimateCostUSD(tc.model, "", u); math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("%s: cost = %v, want %v", tc.model, got, tc.want)
		}
	}
}

func TestUnknownModelUsesTierModelRateNotOpus(t *testing.T) {
	t.Parallel()
	u := Usage{Input: 1e6}
	if got := EstimateCostUSD("claude-mystery-9", "claude-sonnet-5-5", u); got != 2 {
		t.Errorf("unknown model with tier fallback = %v, want tier rate 2", got)
	}
	if got := EstimateCostUSD("glm-5.3", "glm-5.3-flash", u); got != 3 || got >= 15 {
		t.Errorf("unknown everywhere = %v, want neutral sonnet-4 rate 3, never opus", got)
	}
}

func feedCalls(s *Stream, n int, input string, res func(i int) string) {
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("t%d", i)
		s.Feed([]byte(asst(fmt.Sprintf("m%d", i), "claude-sonnet-5-5", 1, 1, use(id, "Bash", input))))
		s.Feed([]byte(result(id, res(i))))
	}
}

func TestLoopCountsOnlyIdenticalCallAndResult(t *testing.T) {
	t.Parallel()
	s := NewStream("")
	feedCalls(s, 3, `{"a": 1}`, func(int) string { return "same" })
	if n, tool := s.Repeats(); n != 3 || tool != "Bash" {
		t.Errorf("identical call+result: repeats = %d %q, want 3 Bash", n, tool)
	}
	// Same call, changing result (polling): resets to 1 every time.
	s = NewStream("")
	feedCalls(s, 5, `{"command":"make ci"}`, func(i int) string { return strings.Repeat("x", i+1) })
	if n, _ := s.Repeats(); n != 1 {
		t.Errorf("polling with changing results: repeats = %d, want 1", n)
	}
	// Different input, same result: not a repeat.
	s = NewStream("")
	s.Feed([]byte(asst("m1", "claude-sonnet-5-5", 1, 1, use("a", "Bash", `{"c":1}`))))
	s.Feed([]byte(result("a", "same")))
	s.Feed([]byte(asst("m2", "claude-sonnet-5-5", 1, 1, use("b", "Bash", `{"c":2}`))))
	s.Feed([]byte(result("b", "same")))
	if n, _ := s.Repeats(); n != 1 {
		t.Errorf("different input: repeats = %d, want 1", n)
	}
	// A call with no result yet does not count.
	s = NewStream("")
	s.Feed([]byte(asst("m1", "claude-sonnet-5-5", 1, 1, use("a", "Bash", `{}`))))
	if n, _ := s.Repeats(); n != 0 {
		t.Errorf("unanswered call: repeats = %d, want 0", n)
	}
}
