package claudecli

import "testing"

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
