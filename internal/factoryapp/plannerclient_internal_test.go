package factoryapp

import (
	"strings"
	"testing"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

func chatEnv(id, payload string) wire.MessageEnvelope {
	return wire.MessageEnvelope{ID: id, Type: "chat.message", From: auth.HumanOperatorIdentity, Payload: []byte(payload)}
}

func TestWakeClient(t *testing.T) {
	mob := chatEnv("m1", `{"text":"a","client":"mobile"}`)
	desk := chatEnv("m2", `{"text":"b","client":"desktop"}`)
	bare := chatEnv("m3", `{"text":"c"}`)
	report := wire.MessageEnvelope{ID: "r1", Type: "foreman.report", Payload: []byte(`{"text":"x"}`)}
	for _, tc := range []struct {
		name           string
		wakers, thread []wire.MessageEnvelope
		want           string
	}{
		{"trigger mobile", []wire.MessageEnvelope{mob}, nil, "mobile"},
		{"newest trigger wins", []wire.MessageEnvelope{mob, desk}, nil, "desktop"},
		{"report wake uses thread", []wire.MessageEnvelope{report}, []wire.MessageEnvelope{mob, report}, "mobile"},
		{"newer unhinted message resets", []wire.MessageEnvelope{bare}, []wire.MessageEnvelope{mob, bare}, ""},
		{"no chat at all", []wire.MessageEnvelope{report}, nil, ""},
	} {
		if got := wakeClient(tc.wakers, tc.thread); got != tc.want {
			t.Errorf("%s: wakeClient = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestWakePromptMobileGuidance(t *testing.T) {
	task := tasks.Task{ID: "task_1"}
	for _, tc := range []struct {
		client string
		want   bool
	}{{"mobile", true}, {"desktop", false}, {"", false}} {
		w := PlannerWake{Reason: "r", Task: task, Client: tc.client}
		for name, p := range map[string]string{"task": buildTaskWakePrompt(w), "standing": buildStandingWakePrompt(w)} {
			has := strings.Contains(p, "operator device: MOBILE")
			if has != tc.want {
				t.Errorf("%s prompt, client %q: mobile section present=%v, want %v", name, tc.client, has, tc.want)
			}
			if tc.want && (!strings.Contains(p, "short") || !strings.Contains(p, "laptop") || !strings.Contains(p, "MUST still be surfaced")) {
				t.Errorf("%s prompt missing guidance:\n%s", name, p)
			}
		}
	}
}
