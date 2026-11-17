package claudecli

import (
	"strings"
	"testing"
)

func TestParseOutput(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantText   string
		wantParsed bool
		wantErr    bool
		want       Usage
	}{
		{
			name: "normal",
			raw: `{"type":"result","subtype":"success","is_error":false,"result":"all done","session_id":"s1",
"total_cost_usd":0.0421,"usage":{"input_tokens":10,"output_tokens":20,"cache_creation_input_tokens":300,"cache_read_input_tokens":4000},
"modelUsage":{"claude-sonnet-5-5":{"inputTokens":10}}}`,
			wantText: "all done", wantParsed: true,
			want: Usage{Input: 10, Output: 20, CacheCreate: 300, CacheRead: 4000, CostUSD: 0.0421, Model: "claude-sonnet-5-5", SessionID: "s1"},
		},
		{
			name:     "event array",
			raw:      `[{"type":"system"},{"type":"result","result":"x","total_cost_usd":1,"usage":{"input_tokens":1,"output_tokens":2}}]`,
			wantText: "x", wantParsed: true, want: Usage{Input: 1, Output: 2, CostUSD: 1},
		},
		{
			name:     "error result",
			raw:      `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Not logged in · Please run /login","usage":{"input_tokens":0,"output_tokens":0}}`,
			wantText: "Not logged in · Please run /login", wantParsed: true, wantErr: true,
		},
		{
			name:     "limit reached",
			raw:      `{"type":"result","subtype":"success","is_error":true,"result":"Claude AI usage limit reached|1763300000","total_cost_usd":0}`,
			wantText: "Claude AI usage limit reached|1763300000", wantParsed: true, wantErr: true,
			want: Usage{Limit: "Claude AI usage limit reached|1763300000"},
		},
		{
			name:     "limit reached newer wording",
			raw:      `{"type":"result","is_error":true,"result":"You've hit your limit · resets 3pm (UTC)"}`,
			wantText: "You've hit your limit · resets 3pm (UTC)", wantParsed: true, wantErr: true,
			want: Usage{Limit: "You've hit your limit · resets 3pm (UTC)"},
		},
		{name: "plain text", raw: "hello world", wantText: "hello world"},
		{name: "garbage json", raw: `{"type":"result", oops`, wantText: `{"type":"result", oops`},
		{name: "unrelated json", raw: `{"foo":1}`, wantText: `{"foo":1}`},
		{name: "empty", raw: "", wantText: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseOutput(tc.raw)
			if got.Text != tc.wantText || got.Parsed != tc.wantParsed || got.IsError != tc.wantErr {
				t.Fatalf("got text=%q parsed=%v err=%v", got.Text, got.Parsed, got.IsError)
			}
			if got.Usage != tc.want {
				t.Fatalf("usage = %+v, want %+v", got.Usage, tc.want)
			}
		})
	}
}

func TestParseOutputStreamJSON(t *testing.T) {
	raw := `{"type":"system","subtype":"init","session_id":"s9"}
{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1794871800,"rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.09,"resetsAt":1794871800,"status":"allowed"},"seven_day":{"utilization":0.06,"resetsAt":1794999600,"status":"allowed"}}}}
{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s9","total_cost_usd":0.01,"usage":{"input_tokens":3,"output_tokens":4}}
`
	o := ParseOutput(raw)
	if !o.Parsed || o.Text != "ok" || o.Usage.Input != 3 || o.Usage.Output != 4 || o.Usage.SessionID != "s9" || o.Usage.CostUSD != 0.01 {
		t.Fatalf("result not used: %+v", o)
	}
	if o.Usage.RateLimit == "" || !strings.Contains(o.Usage.RateLimit, `"seven_day"`) || !strings.Contains(o.Usage.RateLimit, `"resetsAt":1794871800`) {
		t.Fatalf("rate limit not captured: %q", o.Usage.RateLimit)
	}
	// no rate_limit_event: empty; garbage lines skipped; non-result stream -> raw text
	o = ParseOutput("{\"type\":\"system\"}\nnot json\n{\"type\":\"result\",\"result\":\"r\"}\n")
	if !o.Parsed || o.Text != "r" || o.Usage.RateLimit != "" {
		t.Fatalf("%+v", o)
	}
	raw = "{\"type\":\"system\"}\n{\"type\":\"assistant\"}\n"
	if o := ParseOutput(raw); o.Parsed || o.Text != raw {
		t.Fatalf("want raw fallback: %+v", o)
	}
	// array form also carries the event
	o = ParseOutput(`[{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}},{"type":"result","result":"x"}]`)
	if o.Usage.RateLimit != `{"status":"rejected"}` {
		t.Fatalf("array: %q", o.Usage.RateLimit)
	}
}

func TestParseOutputKeepsRejectedEvent(t *testing.T) {
	raw := `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","rateLimitType":"five_hour","resetsAt":1794871800}}
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}
{"type":"result","subtype":"success","is_error":true,"result":"limit reached","session_id":"s"}`
	o := ParseOutput(raw)
	if !strings.Contains(o.Usage.RateLimit, `"rejected"`) {
		t.Fatalf("rejected event overwritten: %q", o.Usage.RateLimit)
	}
	// No result object at all: the rejection is still surfaced.
	o = ParseOutput(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected"}}` + "\n" + `{"type":"system"}`)
	if !strings.Contains(o.Usage.RateLimit, `"rejected"`) || o.Parsed {
		t.Fatalf("no-result rejection: %+v", o)
	}
	// stderr-only limit text is captured.
	u := UsageWithStderr(Usage{}, "Claude AI usage limit reached|1794871800")
	if u.Limit == "" {
		t.Fatal("stderr limit text not captured")
	}
}

func TestParseOutputDuration(t *testing.T) {
	out := ParseOutput(`{"type":"result","result":"ok","session_id":"s","total_cost_usd":0.1,"duration_ms":4321}`)
	if out.Usage.DurationMs != 4321 {
		t.Fatalf("duration = %d, want 4321", out.Usage.DurationMs)
	}
}
