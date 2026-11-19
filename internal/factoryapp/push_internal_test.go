package factoryapp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func TestPushForEnvelope(t *testing.T) {
	long := strings.Repeat("word ", 100)
	tests := []struct {
		name      string
		inbox     string
		env       wire.MessageEnvelope
		ok        bool
		title     string
		urlSuffix string
	}{
		{"planner reply", "planner:main", wire.MessageEnvelope{Type: "brain.reply", ThreadID: "T1", Payload: json.RawMessage(`{"text":"all done"}`)}, true, "Planner", "/#/task/T1"},
		{"reply empty text", "planner:main", wire.MessageEnvelope{Type: "brain.reply", Payload: json.RawMessage(`{}`)}, false, "", ""},
		{"job failed", "planner:main", wire.MessageEnvelope{Type: "notify.job", Payload: json.RawMessage(`{"job_id":"J9","state":"failed","last_error":"boom"}`)}, true, "Job J9 failed", "/#/inbox"},
		{"self cancel is silent", "planner:main", wire.MessageEnvelope{Type: "notify.job", Payload: json.RawMessage(`{"job_id":"J9","state":"cancelled","self_initiated":true}`)}, false, "", ""},
		{"pending", "planner:main", wire.MessageEnvelope{Type: "notify.pending", Payload: json.RawMessage(`{"job_id":"J2","reason":"no foreman"}`)}, true, "Job J2 stuck pending", ""},
		{"operator's own chat", "planner:main", wire.MessageEnvelope{Type: "chat.message", Payload: json.RawMessage(`{"text":"hi"}`)}, false, "", ""},
		{"internal relay", "planner:main", wire.MessageEnvelope{Type: "foreman.report", Payload: json.RawMessage(`{}`)}, false, "", ""},
		{"other inbox", "foreman:a", wire.MessageEnvelope{Type: "brain.reply", Payload: json.RawMessage(`{"text":"x"}`)}, false, "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := pushForEnvelope(tc.inbox, tc.env)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if m.Title != tc.title || !strings.HasSuffix(m.URL, tc.urlSuffix) {
				t.Fatalf("got %+v", m)
			}
		})
	}
	m, _ := pushForEnvelope("planner:main", wire.MessageEnvelope{Type: "brain.reply", Payload: json.RawMessage(`{"text":"` + long + `"}`)})
	if n := len([]rune(m.Body)); n > pushBodyChar {
		t.Fatalf("body not clipped: %d runes", n)
	}
}
