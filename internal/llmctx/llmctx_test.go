package llmctx

import (
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func TestCapPassthrough(t *testing.T) {
	exact := bytesN(CapBytes)
	if got := Cap(exact); string(got) != string(exact) {
		t.Error("exactly-at-cap payload was modified")
	}
	under := []byte(`{"a":1}`)
	if got := Cap(under); string(got) != `{"a":1}` {
		t.Errorf("under-cap payload modified: %s", got)
	}
	if got := Cap(nil); got != nil {
		t.Errorf("nil -> %v, want nil", got)
	}
}

func TestCapTruncation(t *testing.T) {
	payload := bytesN(5000)
	got := Cap(payload)
	s := string(got)
	if !strings.Contains(s, "[truncated, 5000 bytes total]") {
		t.Errorf("marker missing: %q", s[len(s)-80:])
	}
	head := got[:CapBytes]
	// The first 2048 bytes are preserved up to a rune boundary (all ASCII
	// here, so byte-exact).
	if string(head) != string(payload[:CapBytes]) {
		t.Error("first 2048 bytes not preserved")
	}
	if len(s) > CapBytes+64 { // cap + UTF-8-safe cut headroom + marker
		t.Errorf("capped result too long: %d", len(s))
	}
}

func TestCapMultibyteBoundary(t *testing.T) {
	// 2047 ASCII bytes then multibyte runes straddling the 2048 boundary.
	lead := strings.Repeat("x", 2047)
	rune3 := strings.Repeat("é", 40) // 3 bytes each
	payload := []byte(lead + rune3)
	got := string(Cap(payload))
	if strings.HasSuffix(got[:len(got)-40], "é") {
		t.Error("nope")
	}
	// No partial rune: the byte right before the marker must be a rune end.
	idx := strings.Index(got, "\n…[truncated")
	if idx < 0 {
		t.Fatal("marker missing")
	}
	if idx > 0 && got[idx-1]&0xC0 == 0x80 {
		t.Errorf("truncation split a UTF-8 rune at byte %d", idx-1)
	}
	// All bytes before the marker that are part of the payload decode.
	body := got[:idx]
	if !strings.HasSuffix(body, "é") && !strings.HasSuffix(body, "x") {
		t.Errorf("truncated body ends mid-rune: %q", body[len(body)-5:])
	}
}

func TestCapInvalidJSONRaw(t *testing.T) {
	payload := []byte(strings.Repeat("\xff", 3000))
	got := Cap(payload)
	if len(got) <= CapBytes || len(got) > CapBytes+64 {
		t.Errorf("raw-byte cap result length = %d, want just over cap", len(got))
	}
	if !strings.Contains(string(got), "[truncated") {
		t.Error("marker missing on raw path")
	}
}

func TestEnvelopeSnippet(t *testing.T) {
	env := wire.MessageEnvelope{
		ID:      "msg_1",
		Type:    "job.question",
		JobID:   "job_9",
		From:    "worker:job_9",
		TS:      "2026-11-14T12:00:00Z",
		Payload: []byte(`{"answer": "   spaced   "}`),
	}
	s := EnvelopeSnippet(env)
	for _, want := range []string{
		"type: job.question\n",
		"job_id: job_9\n",
		"from: worker:job_9\n",
		"id: msg_1\n",
		"ts: 2026-11-14T12:00:00Z\n",
		`payload: {"answer":"   spaced   "}` + "\n",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("snippet missing %q:\n%s", want, s)
		}
	}

	noJob := wire.MessageEnvelope{Type: "brain.wake", From: "foreman:alpha", Payload: nil}
	s = EnvelopeSnippet(noJob)
	if strings.Contains(s, "job_id") {
		t.Errorf("empty job_id should be omitted:\n%s", s)
	}
	if strings.Contains(s, "payload") {
		t.Errorf("nil payload should be omitted:\n%s", s)
	}
}

func TestBuildWakePromptGolden(t *testing.T) {
	envs := []wire.MessageEnvelope{
		{
			Type: "job.question", JobID: "job_9", From: "worker:job_9",
			Payload: []byte(`{"q":"how big is the file?"}`),
		},
		{Type: "brain.wake", From: "factory:planner"},
	}
	got := BuildWakePrompt("New messages need decisions.", envs)
	if first := BuildWakePrompt("New messages need decisions.", envs); first != got {
		t.Error("prompt is not deterministic")
	}
	for _, want := range []string{
		"foreman brain",
		"New messages need decisions.",
		"--- message 1 ---",
		"--- message 2 ---",
		`{"q":"how big is the file?"}`,
		"Ack what you handled",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
	if p := BuildWakePrompt("planner: review the week", envs); !strings.Contains(p, "factory planner") {
		t.Errorf("planner role not selected:\n%s", p)
	}
}

func TestBuildWakePromptPerEnvelopeCap(t *testing.T) {
	big := []byte(`{"blob":"` + strings.Repeat("A", 3000) + `"}`)
	want := "[truncated, " + itoa(len(big)) + " bytes total]"
	envs := []wire.MessageEnvelope{
		{Type: "job.event", JobID: "job_1", From: "worker:job_1", Payload: big},
		{Type: "job.event", JobID: "job_2", From: "worker:job_2", Payload: big},
	}
	got := BuildWakePrompt("wake", envs)
	if n := strings.Count(got, want); n != 2 {
		t.Errorf("per-envelope caps: %d markers %q, want 2", n, want)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestWakeFooterByteBudget(t *testing.T) {
	if len(wakeFooter) >= 400 {
		t.Errorf("footer boilerplate is %d bytes, keep it under 400", len(wakeFooter))
	}
}

func bytesN(n int) []byte {
	return []byte(strings.Repeat("x", n))
}
