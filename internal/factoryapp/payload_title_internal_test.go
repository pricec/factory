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
