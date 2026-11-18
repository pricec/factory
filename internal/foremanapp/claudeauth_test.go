package foremanapp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pricec/factory/internal/claudeauth"
	"github.com/pricec/factory/internal/wire"
)

const fmToken = "sk-ant-oat01-FOREMANTESTabcdefghijklmnopqrstuvwxyz0123"

// claudeTestApp is a bare App wired for the claude-auth paths: a fake
// factory serving GET /v1/foreman/claude-token and a scripted prober.
func claudeTestApp(t *testing.T, verdict *atomic.Value) (*App, *atomic.Int32) {
	t.Helper()
	var fetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/foreman/claude-token" || r.Header.Get("Authorization") != "Bearer paired" {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		fetches.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"value": fmToken, "rev": "rev1"})
	}))
	t.Cleanup(srv.Close)
	a := &App{
		factoryURL: srv.URL,
		poster:     &Poster{},
		now:        func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) },
		modelCfg:   newModelConfig(modelValues{flash: "openrouter/x", standard: "openrouter/y", hardest: "claude-opus-5-5"}, false),
		claudeAuth: newClaudeAuth(),
	}
	a.paired.Bearer = "paired"
	a.claudeAuth.prober = claudeauth.Prober{Run: func(_ context.Context, _ string, _, env []string, _ string) (string, string, error) {
		switch verdict.Load().(string) {
		case claudeauth.StateOK:
			return `{"is_error":false,"result":"OK"}`, "", nil
		case claudeauth.StateFailed:
			return `{"is_error":true,"result":"Not logged in · Please run /login"}`, "", nil
		}
		return "", "connection reset", context.DeadlineExceeded
	}}
	return a, &fetches
}

func TestClaudeAuthForemanDirective(t *testing.T) {
	ctx := context.Background()
	var verdict atomic.Value
	verdict.Store(claudeauth.StateFailed)
	a, fetches := claudeTestApp(t, &verdict)

	if got := a.claudeTiers(); len(got) != 1 || got[0] != wire.TierHardest {
		t.Fatalf("claudeTiers = %v", got)
	}
	if a.claudeAuth.Env() != nil {
		t.Fatal("no token yet: Env must be nil (host login used)")
	}
	// First verdict: failed (no token).
	if !a.probeClaudeAuth(ctx) || a.claudeAuth.report(nil).State != claudeauth.StateFailed {
		t.Fatalf("probe state: %+v", a.claudeAuth.report(nil))
	}
	// The factory announces a token: fetched once, held, probe forced.
	a.observeClaudeDirective(ctx, &claudeauth.Directive{TokenRev: "rev1"})
	a.observeClaudeDirective(ctx, &claudeauth.Directive{TokenRev: "rev1"})
	if fetches.Load() != 1 {
		t.Fatalf("token fetched %d times", fetches.Load())
	}
	if env := a.claudeAuth.Env(); env[claudeauth.TokenEnv] != fmToken {
		t.Fatalf("Env = %v", env)
	}
	if !a.claudeAuth.forced {
		t.Fatal("a new token must force a probe")
	}
	select {
	case <-a.claudeAuth.kick:
	default:
		t.Fatal("no probe kicked")
	}
	verdict.Store(claudeauth.StateOK)
	if !a.probeClaudeAuth(ctx) {
		t.Fatal("failed -> ok not reported as a change")
	}
	rep := a.claudeAuth.report(a.claudeTiers())
	if rep.State != claudeauth.StateOK || rep.TokenRev != "rev1" || rep.CheckedAt == "" {
		t.Fatalf("report = %+v", rep)
	}
	raw, _ := json.Marshal(rep)
	if strings.Contains(string(raw), fmToken) {
		t.Fatal("token in the caps report")
	}
	// An inconclusive probe never overturns a verdict.
	verdict.Store(claudeauth.StateUnknown)
	if a.probeClaudeAuth(ctx) || a.claudeAuth.report(nil).State != claudeauth.StateOK {
		t.Fatal("inconclusive probe changed the state")
	}
	// Token cleared at the factory: dropped.
	a.observeClaudeDirective(ctx, &claudeauth.Directive{TokenRev: ""})
	if a.claudeAuth.Env() != nil {
		t.Fatal("cleared token still handed out")
	}
}

func TestParseClaudeDirective(t *testing.T) {
	d := parseClaudeDirective(200, []byte(`{"foreman_update":null,"claude_auth":{"token_rev":"r","probe":true}}`))
	if d == nil || d.TokenRev != "r" || !d.Probe {
		t.Fatalf("directive = %+v", d)
	}
	if parseClaudeDirective(204, nil) != nil || parseClaudeDirective(200, []byte(`{"foreman_update":null}`)) != nil {
		t.Fatal("absent directive must be nil")
	}
	// The update advert still parses alongside it.
	if adv, err := parseCapsResponse(200, []byte(`{"claude_auth":{"token_rev":"r"}}`)); err != nil || adv != nil {
		t.Fatalf("advert = %v %v", adv, err)
	}
}

func TestBrainRunnerExtraEnv(t *testing.T) {
	b := &BrainRunner{ExtraEnv: func() map[string]string { return map[string]string{claudeauth.TokenEnv: fmToken} }}
	if b.extraEnv()[claudeauth.TokenEnv] != fmToken {
		t.Fatal("brain extra env")
	}
	if (&BrainRunner{}).extraEnv() != nil {
		t.Fatal("nil ExtraEnv")
	}
}
