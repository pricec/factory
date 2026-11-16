package claudecli

import (
	"math"
	"testing"
)

func TestStreamCostAndLoop(t *testing.T) {
	t.Parallel()
	s := NewStream()
	if _, ok := s.Cost(); ok {
		t.Fatal("cost reported before any usage")
	}
	line := func(id, input string) string {
		return `{"type":"assistant","message":{"id":"` + id + `","model":"claude-sonnet-5-5","usage":{"input_tokens":1000000,"output_tokens":0},"content":[{"type":"tool_use","id":"t","name":"Bash","input":` + input + "}]}}\n"
	}
	// Split mid-line across Feed calls; duplicate message id priced once.
	l := line("m1", `{"a": 1}`)
	s.Feed([]byte(l[:20]))
	s.Feed([]byte(l[20:]))
	s.Feed([]byte(line("m1", `{"a":1}`)))
	if c, ok := s.Cost(); !ok || math.Abs(c-3) > 1e-9 {
		t.Errorf("cost = %v,%v want 3 (1M sonnet input tokens, once)", c, ok)
	}
	if n, tool := s.Repeats(); n != 2 || tool != "Bash" {
		t.Errorf("repeats = %d %q, want 2 Bash (whitespace-insensitive input)", n, tool)
	}
	s.Feed([]byte(line("m2", `{"a":2}`)))
	if n, _ := s.Repeats(); n != 1 {
		t.Errorf("repeats = %d after different input, want 1", n)
	}
	// The result event's total is authoritative.
	s.Feed([]byte(`{"type":"result","total_cost_usd":0.5}` + "\n"))
	if c, _ := s.Cost(); c != 0.5 {
		t.Errorf("cost = %v, want result total 0.5", c)
	}
}
