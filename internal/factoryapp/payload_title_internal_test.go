package factoryapp

import (
	"testing"

	"github.com/pricec/factory/internal/wire"
)

// TestReportTitleFromStringifiedObjectPayload: a foreman.report whose
// object payload arrived as a JSON string titles its task with the text,
// not the raw JSON (deploy-18 regression).
func TestReportTitleFromStringifiedObjectPayload(t *testing.T) {
	payload, err := wire.MarshalPayload(`{"text": "DEPLOY-18 CLOSED, all green"}`)
	if err != nil {
		t.Fatal(err)
	}
	env := wire.MessageEnvelope{Type: "foreman.report", From: "foreman:alpha", Payload: payload}
	if got := envelopeTaskTitle(env); got != "DEPLOY-18 CLOSED, all green" {
		t.Fatalf("title = %q, want the report text", got)
	}
}

// TestReportTitleOldAndNewEncodings: deploy-23 (structured payloads) must
// coexist with the deploy-21 unwrap of legacy double-encoded rows - both
// encodings of the same report title identically.
func TestReportTitleOldAndNewEncodings(t *testing.T) {
	for name, payload := range map[string]string{
		"structured":     `{"text":"DEPLOY-23 CLOSED","task_id":"task_x"}`,
		"double-encoded": `"{\"text\":\"DEPLOY-23 CLOSED\",\"task_id\":\"task_x\"}"`,
	} {
		env := wire.MessageEnvelope{Type: "foreman.report", From: "foreman:alpha", Payload: []byte(payload)}
		if got := envelopeTaskTitle(env); got != "DEPLOY-23 CLOSED" {
			t.Errorf("%s: title = %q, want the report text", name, got)
		}
	}
}
