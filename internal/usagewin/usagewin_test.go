package usagewin

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC)

func TestClassifyTable(t *testing.T) {
	tests := []struct {
		name   string
		util   float64
		pace   float64
		status string
		want   State
	}{
		{"on pace", 20, 1.0, "allowed", Green},
		{"edge below yellow", 20, 1.09, "", Green},
		{"yellow low", 20, 1.10, "", Yellow},
		{"yellow high", 20, 1.30, "", Yellow},
		{"orange low", 20, 1.31, "", Orange},
		{"orange high", 20, 1.60, "", Orange},
		{"red pace", 20, 1.61, "", Red},
		{"tiny util ignores pace", 3, 5.0, "", Green},
		{"orange by util", 86, 0.9, "", Orange},
		{"red by util", 95, 0.9, "", Red},
		{"rejected", 1, 0.1, "rejected", Red},
		{"warning alone is not a state", 10, 1.0, "allowed_warning", Green},
	}
	for _, tc := range tests {
		if got := Classify(tc.util, tc.pace, true, tc.status); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.name, got, tc.want)
		}
	}
	if got := Classify(50, 9, false, ""); got != Green {
		t.Errorf("no pace: %s", got)
	}
}

func TestPaceAndWindowStart(t *testing.T) {
	reset := t0.Add(2 * time.Hour) // 3h of 5h elapsed
	if !WindowStart(reset, FiveHour).Equal(t0.Add(-3 * time.Hour)) {
		t.Fatal("window start")
	}
	p, ok := Pace(30, reset, t0, FiveHour, 1.0)
	if !ok || p < 0.499 || p > 0.501 {
		t.Fatalf("pace=%v ok=%v", p, ok)
	}
	p, _ = Pace(30, reset, t0, FiveHour, 0.5) // half share: allowed halves
	if p < 0.999 || p > 1.001 {
		t.Fatalf("F=.5 pace=%v", p)
	}
	// early window: elapsed floored at 5%
	p, _ = Pace(10, t0.Add(FiveHour), t0, FiveHour, 1.0)
	if p < 1.99 || p > 2.01 {
		t.Fatalf("floor pace=%v", p)
	}
	if _, ok := Pace(10, time.Time{}, t0, FiveHour, 1); ok {
		t.Fatal("zero reset must not be ok")
	}
	if Worse(Green, Red) != Red || Worse(Unknown, Yellow) != Yellow || Worse(Orange, Green) != Orange {
		t.Fatal("worse")
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func TestTrackerChainAndStale(t *testing.T) {
	c := &clock{t0}
	own := int64(0)
	tr := NewTracker(Config{Now: c.now})
	snap := tr.Snapshot(context.Background())
	if snap.State != Unknown {
		t.Fatalf("empty: %s", snap.State)
	}
	// endpoint: 5h 40% with 1h elapsed of 5h (pace 2.0 -> RED), 7d low.
	tr.Set(Win5h, SourceEndpoint, Reading{Utilization: 40, ResetsAt: t0.Add(4 * time.Hour), ObservedAt: t0})
	tr.Set(Win7d, SourceEndpoint, Reading{Utilization: 7, ResetsAt: t0.Add(48 * time.Hour), ObservedAt: t0})
	snap = tr.Snapshot(context.Background())
	if snap.State != Red || snap.Windows[Win5h].Source != SourceEndpoint || *snap.Windows[Win5h].Pace < 1.99 {
		t.Fatalf("snap=%+v", snap.Windows[Win5h])
	}
	if snap.Windows[Win7d].State != Green {
		t.Fatalf("7d=%+v", snap.Windows[Win7d])
	}
	// newer stream reading wins
	c.t = t0.Add(2 * time.Minute)
	if !tr.IngestRateLimit(`{"status":"allowed","unifiedWindows":{"five_hour":{"utilization":0.41,"resetsAt":` + itoa(t0.Add(4*time.Hour).Unix()) + `,"status":"allowed"}}}`) {
		t.Fatal("ingest")
	}
	snap = tr.Snapshot(context.Background())
	if w := snap.Windows[Win5h]; w.Source != SourceStream || *w.Utilization < 40.9 || *w.Utilization > 41.1 {
		t.Fatalf("stream should win: %+v", w)
	}
	// 11 minutes later everything is stale -> UNKNOWN (no caps), values kept
	c.t = t0.Add(13 * time.Minute)
	snap = tr.Snapshot(context.Background())
	if snap.State != Unknown || !snap.Windows[Win5h].Stale || snap.Windows[Win5h].Utilization == nil {
		t.Fatalf("stale: %+v", snap)
	}
	// own accounting fallback needs caps
	tr2 := NewTracker(Config{
		Now: c.now, Cap5h: 1000, Cap7d: 10000,
		Own: func(context.Context, time.Time) (int64, int64, error) { return own, own, nil },
	})
	own = 900
	snap = tr2.Snapshot(context.Background())
	w := snap.Windows[Win5h]
	if w.Source != SourceOwn || *w.Utilization != 90 || w.State != Orange {
		t.Fatalf("own: %+v", w)
	}
	if snap.Windows[Win7d].State != Green {
		t.Fatalf("own 7d: %+v", snap.Windows[Win7d])
	}
}

func itoa(n int64) string { return fmtInt(n) }

func fmtInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

func TestIngestRateLimitRejectedAndGarbage(t *testing.T) {
	c := &clock{t0}
	tr := NewTracker(Config{Now: c.now})
	for _, bad := range []string{"", "nope", `{"unifiedWindows":{"five_hour":{"utilization":"x"}}}`, `{"unifiedWindows":{"five_hour":{"utilization":0.5,"resetsAt":1}}}`} {
		if tr.IngestRateLimit(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	raw := `{"status":"rejected","rateLimitType":"five_hour","unifiedWindows":{"five_hour":{"utilization":0.5,"resetsAt":` +
		fmtInt(t0.Add(time.Hour).Unix()) + `,"status":"allowed"}}}`
	tr.IngestRateLimit(raw)
	if s := tr.Snapshot(context.Background()); s.Windows[Win5h].State != Red {
		t.Fatalf("rejected must be RED: %+v", s.Windows[Win5h])
	}
}

type fakeClient struct {
	status  int
	body    string
	hdr     http.Header
	gotAuth string
	gotBeta string
	calls   int
}

func (f *fakeClient) Do(r *http.Request) (*http.Response, error) {
	f.calls++
	f.gotAuth, f.gotBeta = r.Header.Get("Authorization"), r.Header.Get("anthropic-beta")
	h := f.hdr
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: f.status, Header: h, Body: io.NopCloser(strings.NewReader(f.body))}, nil
}

func creds(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(p, []byte(`{"claudeAiOauth":{"accessToken":"SECRET-TOKEN"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const okBody = `{"five_hour":{"utilization":9.0,"resets_at":"2026-11-16T23:30:00Z"},"seven_day":{"utilization":7.0,"resets_at":"2026-11-18T11:00:00Z"},"tangelo":null,"limits":[{"kind":"session"}]}`

func TestPollerOK401GarbageStale(t *testing.T) {
	c := &clock{t0}
	tr := NewTracker(Config{Now: c.now})
	fc := &fakeClient{status: 200, body: okBody}
	var logs []string
	p := &Poller{
		Tracker: tr, Client: fc, CredsPath: creds(t), Now: c.now, Rand: func() float64 { return 0.5 },
		Logf: func(f string, a ...any) { logs = append(logs, f) },
	}
	if err := p.PollOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fc.gotAuth != "Bearer SECRET-TOKEN" || fc.gotBeta != "oauth-2025-04-20" {
		t.Fatalf("headers %q %q", fc.gotAuth, fc.gotBeta)
	}
	s := tr.Snapshot(context.Background())
	if *s.Windows[Win5h].Utilization != 9 || *s.Windows[Win7d].Utilization != 7 || s.Windows[Win5h].Source != SourceEndpoint {
		t.Fatalf("%+v", s)
	}
	if d := p.Next(s.State, nil); d != 120*time.Second {
		t.Fatalf("green interval %v", d)
	}
	// 401: error, last value kept
	fc.status, fc.body = 401, `{"error":"x"}`
	err := p.PollOnce(context.Background())
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("401 err=%v", err)
	}
	if d := p.Next(Green, err); d != 240*time.Second {
		t.Fatalf("backoff %v", d)
	}
	// 429 with Retry-After
	fc.status, fc.hdr = 429, http.Header{"Retry-After": {"590"}}
	err = p.PollOnce(context.Background())
	if d := p.Next(Green, err); d != 590*time.Second {
		t.Fatalf("retry-after %v", d)
	}
	// garbage + schema violations
	fc.status, fc.hdr = 200, nil
	for _, body := range []string{
		"<html>", `{"five_hour":{"utilization":"9","resets_at":"2026-11-16T23:30:00Z"},"seven_day":{"utilization":1,"resets_at":"2026-11-18T11:00:00Z"}}`,
		`{"five_hour":{"utilization":9},"seven_day":{"utilization":1,"resets_at":"2026-11-18T11:00:00Z"}}`, `{}`,
	} {
		fc.body = body
		if err := p.PollOnce(context.Background()); err == nil {
			t.Errorf("accepted %q", body)
		}
	}
	// still the last good value; then stale after 10+ min
	if s := tr.Snapshot(context.Background()); *s.Windows[Win5h].Utilization != 9 || s.Windows[Win5h].Stale {
		t.Fatalf("kept: %+v", s.Windows[Win5h])
	}
	c.t = t0.Add(11 * time.Minute)
	s = tr.Snapshot(context.Background())
	if !s.Windows[Win5h].Stale || *s.Windows[Win5h].Utilization != 9 || s.State != Unknown {
		t.Fatalf("stale: %+v", s)
	}
	// failures never leak the token
	for _, l := range logs {
		if strings.Contains(l, "SECRET") {
			t.Fatal("token logged")
		}
	}
	// YELLOW+ polls faster; success resets backoff
	if d := p.Next(Yellow, nil); d != 60*time.Second {
		t.Fatalf("yellow %v", d)
	}
}

func TestPollerNoCreds(t *testing.T) {
	tr := NewTracker(Config{})
	p := &Poller{Tracker: tr, Client: &fakeClient{status: 200, body: okBody}, CredsPath: filepath.Join(t.TempDir(), "none")}
	if err := p.PollOnce(context.Background()); err == nil {
		t.Fatal("want error")
	}
}
