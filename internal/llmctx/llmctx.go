// Package llmctx implements the LLM context builder (spec §3.3 point 3,
// §6.6): any payload entering an LLM context is hard-capped at 2KB per
// payload, except explicit read_result pulls (#19).
//
// Enforcement-point ownership: (1) summary-required-at-report lives in
// internal/wire + internal/mcpworker; (2) byte-identical-forward-then-store
// lives in internal/routing + internal/jobs; (3) the 2KB context cap lives
// here.
package llmctx

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/pricec/factory/internal/wire"
)

// CapBytes is the per-payload LLM context cap (spec §6.6: 2KB).
const CapBytes = 2048

const truncMarkerFmt = "\n…[truncated, %d bytes total]"

// Cap returns payload unchanged when it fits the 2KB cap; otherwise it
// returns a truncated copy cut at a UTF-8-safe boundary with a trailing
// marker noting the full size. The marker may push the result a few bytes
// past CapBytes - the caller bounds the prompt, not the byte-exact total.
// Nil returns nil. It never splits a UTF-8 rune.
func Cap(payload []byte) []byte {
	if payload == nil {
		return nil
	}
	if len(payload) <= CapBytes {
		return payload
	}
	cut := CapBytes
	for cut > 0 && !utf8Start(payload[cut]) {
		cut--
	}
	marker := []byte(fmt.Sprintf(truncMarkerFmt, len(payload)))
	out := make([]byte, 0, cut+len(marker))
	out = append(out, payload[:cut]...)
	return append(out, marker...)
}

// utf8Start reports whether b is a UTF-8 continuation byte (10xxxxxx).
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// EnvelopeSnippet renders an envelope for inclusion in a brain or planner
// prompt: every field on its own line, payload compacted when valid JSON
// and capped via Cap. Deterministic.
func EnvelopeSnippet(env wire.MessageEnvelope) string {
	var b strings.Builder
	fmt.Fprintf(&b, "type: %s\n", env.Type)
	if env.JobID != "" {
		fmt.Fprintf(&b, "job_id: %s\n", env.JobID)
	}
	fmt.Fprintf(&b, "from: %s\n", env.From)
	if env.ID != "" {
		fmt.Fprintf(&b, "id: %s\n", env.ID)
	}
	if env.TS != "" {
		fmt.Fprintf(&b, "ts: %s\n", env.TS)
	}
	if len(env.Payload) > 0 {
		payload := compact(env.Payload)
		b.WriteString("payload: ")
		b.Write(Cap(payload))
		b.WriteString("\n")
	}
	return b.String()
}

// BuildWakePrompt builds the prompt for an LLM brain session woken to
// handle envs (foreman brain or factory planner). Each envelope's payload
// is independently capped (the 2KB cap is per payload, not per prompt);
// the caller bounds the batch via the inbox fetch limit.
func BuildWakePrompt(reason string, envs []wire.MessageEnvelope) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are the %s. %s\n", roleLine(reason), reason)
	if len(envs) == 0 {
		b.WriteString("No triggering messages.\n")
	}
	for i, env := range envs {
		fmt.Fprintf(&b, "\n--- message %d ---\n%s", i+1, EnvelopeSnippet(env))
	}
	b.WriteString(wakeFooter)
	return b.String()
}

const wakeFooter = `
Handle the messages above with your tools, then finish. Ack what you handled; do not repeat work.`

func roleLine(reason string) string {
	if strings.Contains(reason, "planner") {
		return "factory planner"
	}
	return "foreman brain"
}

// compact strips insignificant whitespace from valid JSON; invalid input
// returns unchanged (raw bytes path).
func compact(data []byte) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		return data
	}
	return buf.Bytes()
}
