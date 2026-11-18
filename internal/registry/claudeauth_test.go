package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRefreshHandlerExtension (spec §13.3): the claude_auth report reaches
// the extension, whose fields join the response (turning 204 into 200);
// an absent report passes nil.
func TestRefreshHandlerExtension(t *testing.T) {
	authz := func(_ *http.Request, id *string) error { *id = "foreman:alpha"; return nil }
	upsert := func(context.Context, string, map[string]string) error { return nil }
	var seen json.RawMessage
	ext := func(_ context.Context, id string, raw json.RawMessage) map[string]any {
		seen = raw
		if raw == nil {
			return nil
		}
		return map[string]any{"claude_auth": map[string]any{"token_rev": "r1", "probe": true}}
	}
	h := RefreshHandlerExtended(authz, upsert, nil, nil, ext)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"capabilities":{},"claude_auth":{"state":"failed","claude_tiers":["hardest"]}}`)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"token_rev":"r1"`) {
		t.Fatalf("with report: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(string(seen), `"state":"failed"`) {
		t.Fatalf("extension saw %s", seen)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("POST", "/", strings.NewReader(`{"capabilities":{}}`)))
	if rec.Code != 204 || seen != nil {
		t.Fatalf("without report: %d seen=%s", rec.Code, seen)
	}
}

func TestMatchExcept(t *testing.T) {
	m := &CapabilityMatcher{Source: claudeStubSource{{ID: "foreman:a", Online: true}, {ID: "foreman:b", Online: true}}}
	f, ok, err := m.MatchExcept(context.Background(), nil, func(f Foreman) bool { return f.ID == "foreman:a" })
	if err != nil || !ok || f.ID != "foreman:b" {
		t.Fatalf("MatchExcept = %v %v %v", f.ID, ok, err)
	}
	if _, ok, _ := m.MatchExcept(context.Background(), nil, func(Foreman) bool { return true }); ok {
		t.Fatal("all excluded must not match")
	}
}

type claudeStubSource []Foreman

func (s claudeStubSource) Foremen(context.Context) ([]Foreman, error) { return s, nil }
