package mcpworker_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

// postedResult returns the single JobResult report_result posted into the
// foreman inbox during the test.
func postedResult(t *testing.T, e *env) wire.JobResult {
	t.Helper()
	envs, err := e.svc.Get(context.Background(), foremanInbox, 10, 0)
	if err != nil {
		t.Fatalf("inbox.Get: %v", err)
	}
	if len(envs) != 1 {
		t.Fatalf("foreman inbox has %d envelopes, want 1", len(envs))
	}
	var jr wire.JobResult
	if err := json.Unmarshal(envs[0].Payload, &jr); err != nil {
		t.Fatalf("payload not a JobResult: %v", err)
	}
	return jr
}

// failedArgs builds a status=failed report; errVal is set as the error
// argument unless it is the sentinel omitError.
func failedArgs(errVal any) map[string]any {
	args := map[string]any{"status": "failed", "summary": "the job failed"}
	if errVal != omitError {
		args["error"] = errVal
	}
	return args
}

// omitError marks a report with no error argument at all.
var omitError = &struct{ omit bool }{true}

// TestReportResultFailedErrorForms covers the status=failed error shapes the
// server accepts: the pinned object form as-is, and the normalized forms
// (string, wrong-case or alias kinds, unknown kinds, missing error).
func TestReportResultFailedErrorForms(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want wire.JobError
	}{
		{
			name: "object error accepted",
			args: failedArgs(map[string]any{"kind": "timeout", "message": "ran 30m"}),
			want: wire.JobError{Kind: wire.KindTimeout, Message: "ran 30m"},
		},
		{
			name: "string error wrapped as other",
			args: failedArgs("boom"),
			want: wire.JobError{Kind: wire.KindOther, Message: "boom"},
		},
		{
			name: "kind Timeout (case) maps to timeout",
			args: failedArgs(map[string]any{"kind": "Timeout", "message": "m"}),
			want: wire.JobError{Kind: wire.KindTimeout, Message: "m"},
		},
		{
			name: "kind time maps to timeout",
			args: failedArgs(map[string]any{"kind": "time", "message": "m"}),
			want: wire.JobError{Kind: wire.KindTimeout, Message: "m"},
		},
		{
			name: "kind VALIDATION (case) maps to validation",
			args: failedArgs(map[string]any{"kind": "VALIDATION", "message": "m"}),
			want: wire.JobError{Kind: wire.KindValidation, Message: "m"},
		},
		{
			name: "kind invalid maps to validation",
			args: failedArgs(map[string]any{"kind": "invalid", "message": "m"}),
			want: wire.JobError{Kind: wire.KindValidation, Message: "m"},
		},
		{
			name: "kind Crash (case) maps to crash",
			args: failedArgs(map[string]any{"kind": "Crash", "message": "m"}),
			want: wire.JobError{Kind: wire.KindCrash, Message: "m"},
		},
		{
			name: "kind LLM (case) maps to llm",
			args: failedArgs(map[string]any{"kind": "LLM", "message": "m"}),
			want: wire.JobError{Kind: wire.KindLLM, Message: "m"},
		},
		{
			name: "unknown kind error maps to other, original kept",
			args: failedArgs(map[string]any{"kind": "error", "message": "parse failed"}),
			want: wire.JobError{Kind: wire.KindOther, Message: `original kind "error": parse failed`},
		},
		{
			name: "unknown kind failed with no message keeps original kind",
			args: failedArgs(map[string]any{"kind": "failed"}),
			want: wire.JobError{Kind: wire.KindOther, Message: `original kind "failed"`},
		},
		{
			name: "object without kind maps to other",
			args: failedArgs(map[string]any{"message": "no kind given"}),
			want: wire.JobError{Kind: wire.KindOther, Message: "no kind given"},
		},
		{
			name: "missing error synthesized",
			args: failedArgs(omitError),
			want: wire.JobError{Kind: wire.KindOther, Message: "status=failed with no error field"},
		},
		{
			name: "null error synthesized",
			args: failedArgs(nil),
			want: wire.JobError{Kind: wire.KindOther, Message: "status=failed with no error field"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			sc := structured(t, resultOf(t, e.callTool(tc.args)))
			if sc["ok"] != true {
				t.Fatalf("structuredContent = %v, want ok:true", sc)
			}
			jr := postedResult(t, e)
			if jr.Status != wire.StatusFailed {
				t.Fatalf("status = %q, want failed", jr.Status)
			}
			if jr.Error == nil {
				t.Fatalf("posted error = nil, want %+v", tc.want)
			}
			if *jr.Error != tc.want {
				t.Fatalf("posted error = %+v, want %+v", *jr.Error, tc.want)
			}
		})
	}
}

// TestReportResultRejects: validation the normalization must not loosen.
// Summary and status rules are wire.JobResult.Validate's, unchanged; a
// non-string, non-object error is rejected rather than guessed at.
func TestReportResultRejects(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{
			name: "summary over 240 chars",
			args: map[string]any{"status": "done", "summary": strings.Repeat("x", 241)},
			want: "summary",
		},
		{
			name: "failed with empty summary",
			args: map[string]any{"status": "failed", "summary": "", "error": "boom"},
			want: "summary",
		},
		{
			name: "status case-sensitive",
			args: map[string]any{"status": "Failed", "summary": "x", "error": "boom"},
			want: "status",
		},
		{
			name: "error that is a number",
			args: failedArgs(42),
			want: "error",
		},
		{
			name: "error that is an array",
			args: failedArgs([]any{"boom"}),
			want: "error",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			msg := toolError(t, e.callTool(tc.args))
			if !strings.Contains(msg, tc.want) {
				t.Errorf("tool error = %q, want mention of %q", msg, tc.want)
			}
		})
	}
}

// TestReportResultDoneResult: the done path carries its result through
// unchanged and posts no error. The worker tool validates without the job's
// result_schema (wire Validate(nil), as before), so this asserts acceptance
// and pass-through; schema conformance is wire.Validate's job.
func TestReportResultDoneResult(t *testing.T) {
	e := newEnv(t)
	args := map[string]any{
		"status":  "done",
		"summary": "classified 3 items",
		"result":  map[string]any{"labels": []any{"a", "b", "c"}},
	}
	sc := structured(t, resultOf(t, e.callTool(args)))
	if sc["ok"] != true {
		t.Fatalf("structuredContent = %v, want ok:true", sc)
	}
	jr := postedResult(t, e)
	if jr.Status != wire.StatusDone || jr.Error != nil {
		t.Fatalf("JobResult = %+v, want done with no error", jr)
	}
	var got, want any
	if err := json.Unmarshal(jr.Result, &got); err != nil {
		t.Fatalf("posted result: %v", err)
	}
	if err := json.Unmarshal([]byte(`{"labels":["a","b","c"]}`), &want); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(want)
	if string(gb) != string(wb) {
		t.Fatalf("posted result = %s, want %s", gb, wb)
	}
}

// TestReportResultAdvertisedSchema: the advertised input schema pins the
// object form of error (kind enum of the five pinned kinds) and the status
// enum, and requires status and summary.
func TestReportResultAdvertisedSchema(t *testing.T) {
	e := newEnv(t)
	_, out := e.rpc(e.worker, "tools/list", nil)
	tools := resultOf(t, out)["tools"].([]any)
	var schema map[string]any
	for _, tl := range tools {
		tm := tl.(map[string]any)
		if tm["name"] == "report_result" {
			schema = tm["inputSchema"].(map[string]any)
		}
	}
	if schema == nil {
		t.Fatal("report_result not advertised")
	}
	props := schema["properties"].(map[string]any)
	errSchema := props["error"].(map[string]any)
	if errSchema["type"] != "object" {
		t.Fatalf("error schema type = %v, want object", errSchema["type"])
	}
	kinds := errSchema["properties"].(map[string]any)["kind"].(map[string]any)["enum"]
	wantKinds := []any{"validation", "timeout", "crash", "llm", "other", "superseded"}
	if b1, b2 := mustJSON(t, kinds), mustJSON(t, wantKinds); b1 != b2 {
		t.Fatalf("kind enum = %s, want %s", b1, b2)
	}
	statusEnum := props["status"].(map[string]any)["enum"]
	if b1, b2 := mustJSON(t, statusEnum), mustJSON(t, []any{"done", "failed"}); b1 != b2 {
		t.Fatalf("status enum = %s, want %s", b1, b2)
	}
	if b1, b2 := mustJSON(t, schema["required"]), mustJSON(t, []any{"status", "summary"}); b1 != b2 {
		t.Fatalf("required = %s, want %s", b1, b2)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
