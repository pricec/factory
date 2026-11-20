package factoryapp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

func TestResetContext(t *testing.T) {
	for _, via := range []string{"command", "endpoint"} {
		t.Run(via, func(t *testing.T) {
			a := newInternalApp(t)
			ctx := context.Background()
			post := func(text string) wire.MessageEnvelope {
				env, err := a.postToTask(ctx, tasks.StandingID, wire.MessageEnvelope{
					Type: "chat.message", From: "human:operator", Payload: []byte(`{"text":"` + text + `"}`),
				})
				if err != nil {
					t.Fatal(err)
				}
				return env
			}
			post("OLD-MARK-1")
			post("OLD-MARK-2")
			if err := a.tasks.SetStandingSummary(ctx, "SUMMARY-MARK", "x"); err != nil {
				t.Fatal(err)
			}
			if err := a.tasks.SetPriorityCriteria(ctx, "CRIT-MARK"); err != nil {
				t.Fatal(err)
			}

			var rec *httptest.ResponseRecorder
			if via == "command" {
				rec = httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/v1/chat", strings.NewReader(`{"text":" /Reset ","task_id":"`+tasks.StandingID+`"}`))
				a.handleChat(rec, req)
				if rec.Code != http.StatusCreated {
					t.Fatalf("/reset = %d %s", rec.Code, rec.Body)
				}
			} else {
				rec = httptest.NewRecorder()
				req := httptest.NewRequest("POST", "/v1/tasks/"+tasks.StandingID+"/reset_context", nil)
				req.SetPathValue("id", tasks.StandingID)
				a.handleResetContext(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("reset_context = %d %s", rec.Code, rec.Body)
				}
			}

			// Summary cleared.
			if sum, through, err := a.tasks.StandingSummary(ctx); err != nil || sum != "" || through != "" {
				t.Errorf("summary = %q/%q err %v, want cleared", sum, through, err)
			}
			// Messages still stored, plus the visible note.
			all, err := a.inbox.ThreadMessages(ctx, tasks.StandingID, 100)
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			for _, m := range all {
				text.Write(m.Payload)
			}
			for _, want := range []string{"OLD-MARK-1", "OLD-MARK-2", contextClearedText} {
				if !strings.Contains(text.String(), want) {
					t.Errorf("stored thread lacks %q", want)
				}
			}

			// Next wake: pre-cutoff messages excluded, criteria kept.
			fresh := post("NEW-MARK")
			var prompt string
			a.SetPlannerExec(func(_ context.Context, p string) (string, error) { prompt = p; return "", nil })
			if err := a.runTaskWake(ctx, tasks.StandingID, []wire.MessageEnvelope{fresh}); err != nil {
				t.Fatal(err)
			}
			for _, bad := range []string{"OLD-MARK-1", "OLD-MARK-2", "SUMMARY-MARK"} {
				if strings.Contains(prompt, bad) {
					t.Errorf("prompt still contains %q", bad)
				}
			}
			for _, want := range []string{"NEW-MARK", "CRIT-MARK"} {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt lacks %q", want)
				}
			}
		})
	}
}

func TestAfterCutoff(t *testing.T) {
	th := []wire.MessageEnvelope{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	for _, tc := range []struct {
		cutoff string
		want   int
	}{{"", 3}, {"a", 2}, {"c", 0}, {"older-than-window", 3}} {
		if got := len(afterCutoff(th, tc.cutoff)); got != tc.want {
			t.Errorf("cutoff %q: %d rows, want %d", tc.cutoff, got, tc.want)
		}
	}
}
