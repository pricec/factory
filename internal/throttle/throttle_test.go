package throttle

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/usagewin"
)

func TestRule(t *testing.T) {
	job := func(p0 bool, prio int) Request { return Request{Kind: KindJob, P0: p0, JobPriority: prio} }
	chat := Request{Kind: KindChatWake}
	recent := Load{HasLastStart: true, SinceLastStart: 10 * time.Second}
	cases := []struct {
		name  string
		level usagewin.State
		req   Request
		load  Load
		hold  bool
	}{
		{"green admits", usagewin.Green, job(false, 9), recent, false},
		{"unknown fails open", usagewin.Unknown, job(false, 9), Load{ActiveNonP0: 9}, false},
		{"green ignores queue", usagewin.Green, job(false, 5), Load{QueuedAhead: 3}, false},
		{"yellow first start", usagewin.Yellow, job(false, 5), Load{}, false},
		{"yellow spacing holds", usagewin.Yellow, job(false, 1), recent, true},
		{"yellow spacing elapsed", usagewin.Yellow, job(false, 5), Load{HasLastStart: true, SinceLastStart: 61 * time.Second}, false},
		{"yellow P0 not spaced", usagewin.Yellow, job(true, 9), recent, false},
		{"orange holds prio 5", usagewin.Orange, job(false, 5), Load{}, true},
		{"orange holds prio 9", usagewin.Orange, job(false, 9), Load{}, true},
		{"orange admits prio 4", usagewin.Orange, job(false, 4), Load{ActiveNonP0: 1}, false},
		{"orange cap 2", usagewin.Orange, job(false, 1), Load{ActiveNonP0: 2}, true},
		{"orange queue", usagewin.Orange, job(false, 1), Load{QueuedAhead: 1}, true},
		{"orange P0 bypass", usagewin.Orange, job(true, 9), Load{ActiveNonP0: 5}, false},
		{"red holds prio 1", usagewin.Red, job(false, 1), Load{}, true},
		{"red P0 bypass", usagewin.Red, job(true, 9), Load{ActiveNonP0: 9, QueuedAhead: 9}, false},
		{"red chat wake bypass", usagewin.Red, chat, Load{}, false},
		{"orange chat wake bypass", usagewin.Orange, chat, Load{ActiveNonP0: 9}, false},
	}
	for _, c := range cases {
		hold, why := Rule(c.level, c.req, c.load)
		if hold != c.hold {
			t.Errorf("%s: hold=%v (%s), want %v", c.name, hold, why, c.hold)
		}
		if hold && why == "" {
			t.Errorf("%s: hold without a reason", c.name)
		}
	}
}

// fixture drives a Gate with a fake clock and a settable 5h reading.
type fixture struct {
	now    time.Time
	util   float64
	state  usagewin.State
	source usagewin.Source
	stale  bool
	reset  time.Time
	alerts []string
	logs   []string
	g      *Gate
}

func newFixture(envOff bool) *fixture {
	f := &fixture{
		now: time.Date(2026, 11, 16, 20, 0, 0, 0, time.UTC), state: usagewin.Green,
		source: usagewin.SourceEndpoint, util: 10,
	}
	f.reset = f.now.Add(3 * time.Hour)
	f.g = New(Config{
		Now:    func() time.Time { return f.now },
		EnvOff: envOff,
		Snapshot: func(context.Context) usagewin.Snapshot {
			u, r, age := f.util, f.reset, int64(0)
			return usagewin.Snapshot{Windows: map[usagewin.Window]usagewin.WindowView{
				usagewin.Win5h: {Utilization: &u, ResetsAt: &r, State: f.state, Source: f.source, Stale: f.stale, AgeS: &age},
			}}
		},
		OnAlert: func(_ context.Context, l usagewin.State, title, _ string) {
			f.alerts = append(f.alerts, string(l)+"|"+title)
		},
		Logf: func(format string, a ...any) { f.logs = append(f.logs, format) },
	})
	return f
}

func (f *fixture) admit(id string, p0 bool, prio int) (bool, string) {
	return f.g.Admit(context.Background(), id, Request{Kind: KindJob, P0: p0, JobPriority: prio},
		func(context.Context) ([]string, error) { return nil, nil }, 0)
}

func TestGateRedHoldsNonP0AdmitsP0(t *testing.T) {
	f := newFixture(false)
	f.state, f.util = usagewin.Red, 96
	hold, reason := f.admit("j1", false, 1)
	if !hold || !strings.HasPrefix(reason, "usage RED: 5h 96%, resets 23:00Z") {
		t.Fatalf("red non-P0: hold=%v reason=%q", hold, reason)
	}
	if hold, _ := f.admit("j2", true, 9); hold {
		t.Fatal("P0 held in RED")
	}
}

func TestGateHysteresis(t *testing.T) {
	f := newFixture(false)
	f.state = usagewin.Orange
	if ev := f.g.Evaluate(context.Background()); ev.Level != usagewin.Orange {
		t.Fatalf("level %s", ev.Level)
	}
	// Raw drops to GREEN: the level holds ORANGE for 10 min.
	f.state = usagewin.Green
	for _, step := range []time.Duration{0, 5 * time.Minute, 4*time.Minute + 59*time.Second} {
		f.now = f.now.Add(step)
		if ev := f.g.Evaluate(context.Background()); ev.Level != usagewin.Orange || ev.Raw != usagewin.Green {
			t.Fatalf("after %v: level %s raw %s", step, ev.Level, ev.Raw)
		}
	}
	if hold, _ := f.admit("j", false, 5); !hold {
		t.Fatal("prio 5 admitted during hysteresis")
	}
	// A blip back to ORANGE resets the clock.
	f.state = usagewin.Orange
	f.g.Evaluate(context.Background())
	f.state = usagewin.Green
	f.g.Evaluate(context.Background()) // below again from here
	f.now = f.now.Add(9 * time.Minute)
	if ev := f.g.Evaluate(context.Background()); ev.Level != usagewin.Orange {
		t.Fatalf("blip did not reset: %s", ev.Level)
	}
	f.now = f.now.Add(time.Minute) // 10 min below since the blip ended
	if ev := f.g.Evaluate(context.Background()); ev.Level != usagewin.Green {
		t.Fatalf("level after 10 min below: %s", ev.Level)
	}
	// Escalation is immediate.
	f.state = usagewin.Red
	if ev := f.g.Evaluate(context.Background()); ev.Level != usagewin.Red {
		t.Fatalf("escalate: %s", ev.Level)
	}
	// RED -> YELLOW drops to the worst level seen below RED (YELLOW).
	f.state = usagewin.Yellow
	f.g.Evaluate(context.Background())
	f.now = f.now.Add(10 * time.Minute)
	if ev := f.g.Evaluate(context.Background()); ev.Level != usagewin.Yellow {
		t.Fatalf("RED->YELLOW: %s", ev.Level)
	}
}

func TestGateFailOpen(t *testing.T) {
	cases := []struct {
		name string
		mut  func(f *fixture)
	}{
		{"stale", func(f *fixture) { f.stale = true }},
		{"unknown", func(f *fixture) { f.state = usagewin.Unknown }},
		{"own accounting only", func(f *fixture) { f.source = usagewin.SourceOwn }},
	}
	for _, c := range cases {
		f := newFixture(false)
		f.state, f.util = usagewin.Red, 99
		c.mut(f)
		ev := f.g.Evaluate(context.Background())
		if !ev.FailOpen || ev.Level != usagewin.Green {
			t.Errorf("%s: %+v", c.name, ev)
		}
		if hold, _ := f.admit("j", false, 9); hold {
			t.Errorf("%s: held while failing open", c.name)
		}
		if len(f.logs) == 0 {
			t.Errorf("%s: fail-open not logged", c.name)
		}
	}
	// No snapshot at all (dead poller, no tracker).
	g := New(Config{Logf: func(string, ...any) {}})
	if hold, _ := g.Admit(context.Background(), "j", Request{Kind: KindJob, JobPriority: 9}, nil, 0); hold {
		t.Fatal("nil snapshot held")
	}
}

func TestGateYellowSpacingAndOrangeCap(t *testing.T) {
	f := newFixture(false)
	f.state = usagewin.Yellow
	if hold, _ := f.admit("a", false, 5); hold {
		t.Fatal("first yellow start held")
	}
	if hold, _ := f.admit("b", false, 5); !hold {
		t.Fatal("second yellow start within 60s admitted")
	}
	if hold, _ := f.admit("p0", true, 5); hold {
		t.Fatal("P0 spaced")
	}
	f.now = f.now.Add(60 * time.Second)
	if hold, _ := f.admit("b", false, 5); hold {
		t.Fatal("yellow start after 60s held")
	}

	// ORANGE: in-flight admissions count toward the cap of 2 until the
	// DB shows them active.
	f = newFixture(false)
	f.state = usagewin.Orange
	active := []string{}
	adm := func(id string) bool {
		h, _ := f.g.Admit(context.Background(), id, Request{Kind: KindJob, JobPriority: 3},
			func(context.Context) ([]string, error) { return active, nil }, 0)
		return h
	}
	if adm("x") || adm("y") {
		t.Fatal("first two held")
	}
	if !adm("z") {
		t.Fatal("third admitted past cap")
	}
	active = []string{"x"} // y finished, x running
	f.now = f.now.Add(3 * time.Minute)
	if adm("z") {
		t.Fatal("z held with 1 active")
	}
}

func TestGateProjectedExhaustBump(t *testing.T) {
	f := newFixture(false)
	f.reset = f.now.Add(4 * time.Hour)
	f.util = 40
	// 2%/min burn: 60% left exhausts in 30 min, well before the reset.
	for i := 0; i <= 12; i++ {
		f.g.Evaluate(context.Background())
		f.now = f.now.Add(time.Minute)
		f.util += 2
	}
	ev := f.g.Evaluate(context.Background())
	if ev.Raw != usagewin.Yellow || len(ev.Bumped) != 1 || !strings.Contains(ev.Summary, "projected") {
		t.Fatalf("bump: %+v", ev)
	}
	// Flat usage: no bump (new window resets history).
	f2 := newFixture(false)
	for i := 0; i <= 12; i++ {
		f2.g.Evaluate(context.Background())
		f2.now = f2.now.Add(time.Minute)
	}
	if ev := f2.g.Evaluate(context.Background()); ev.Raw != usagewin.Green || len(ev.Bumped) != 0 {
		t.Fatalf("flat: %+v", ev)
	}
}

func TestGateAlertsDedupedPerWindow(t *testing.T) {
	f := newFixture(false)
	f.state = usagewin.Orange
	f.g.Evaluate(context.Background())
	f.state = usagewin.Red
	f.g.Evaluate(context.Background())
	if len(f.alerts) != 2 || !strings.HasPrefix(f.alerts[0], "ORANGE|") || !strings.HasPrefix(f.alerts[1], "RED|") {
		t.Fatalf("alerts: %v", f.alerts)
	}
	// Drop to GREEN and re-enter in the SAME window: no new alert.
	f.state = usagewin.Green
	f.g.Evaluate(context.Background())
	f.now = f.now.Add(11 * time.Minute)
	f.g.Evaluate(context.Background())
	f.state = usagewin.Red
	f.g.Evaluate(context.Background())
	if len(f.alerts) != 2 {
		t.Fatalf("re-alerted in same window: %v", f.alerts)
	}
	// New window (reset moved): alert again.
	f.state = usagewin.Green
	f.g.Evaluate(context.Background())
	f.now = f.now.Add(11 * time.Minute)
	f.g.Evaluate(context.Background())
	f.reset = f.reset.Add(5 * time.Hour)
	f.state = usagewin.Red
	f.g.Evaluate(context.Background())
	if len(f.alerts) != 3 {
		t.Fatalf("new window not alerted: %v", f.alerts)
	}
}

type memOverrides struct{ raw string }

func (m *memOverrides) LoadOverride(context.Context) (string, bool, error) {
	return m.raw, m.raw != "", nil
}

func (m *memOverrides) SaveOverride(_ context.Context, raw string) error { m.raw = raw; return nil }

func TestGateKillSwitchAndOverride(t *testing.T) {
	f := newFixture(true)
	f.state = usagewin.Red
	if hold, _ := f.admit("j", false, 9); hold {
		t.Fatal("env off held")
	}
	if ev := f.g.Evaluate(context.Background()); ev.Mode != ModeOff || ev.ModeSource != "env" {
		t.Fatalf("env eval %+v", ev)
	}

	f = newFixture(false)
	store := &memOverrides{}
	f.g.cfg.Overrides = store
	f.state = usagewin.Red
	if _, err := f.g.SetOverride(context.Background(), "bogus", time.Time{}); err == nil {
		t.Fatal("bogus mode accepted")
	}
	if _, err := f.g.SetOverride(context.Background(), ModeOff, f.now.Add(-time.Minute)); err == nil {
		t.Fatal("past until accepted")
	}
	ev, err := f.g.SetOverride(context.Background(), ModeOff, f.now.Add(30*time.Minute))
	if err != nil || ev.Mode != ModeOff || ev.ModeSource != "override" || store.raw == "" {
		t.Fatalf("override: %+v %v %q", ev, err, store.raw)
	}
	if hold, _ := f.admit("j", false, 9); hold {
		t.Fatal("held under override")
	}
	// Persisted: a fresh gate over the same store honours it.
	g2 := New(Config{Now: func() time.Time { return f.now }, Overrides: store, Logf: func(string, ...any) {}})
	if ev := g2.Evaluate(context.Background()); ev.Mode != ModeOff {
		t.Fatalf("override not reloaded: %+v", ev)
	}
	// Expires automatically.
	f.now = f.now.Add(31 * time.Minute)
	if hold, _ := f.admit("j", false, 9); !hold {
		t.Fatal("override did not expire")
	}
	if store.raw != "" {
		t.Fatalf("expired override still persisted: %q", store.raw)
	}
	// auto clears; default until is 1h; huge until is capped at 7d.
	ev, _ = f.g.SetOverride(context.Background(), ModeOff, time.Time{})
	if got := ev.OverrideUntil.Sub(f.now); got != DefaultOverride {
		t.Fatalf("default until %v", got)
	}
	ev, _ = f.g.SetOverride(context.Background(), ModeOff, f.now.Add(30*24*time.Hour))
	if got := ev.OverrideUntil.Sub(f.now); got != MaxOverride {
		t.Fatalf("cap %v", got)
	}
	if ev, _ := f.g.SetOverride(context.Background(), ModeAuto, time.Time{}); ev.Mode != ModeAuto {
		t.Fatalf("auto: %+v", ev)
	}
}
