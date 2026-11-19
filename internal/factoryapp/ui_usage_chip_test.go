package factoryapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUIUsageRingsWiring: the top bar carries two usage rings (5h, 7d) in
// both the phone bar and the desktop header, fed by GET /v1/usage keys the
// backend emits, with aria labels and a non-colour status signal.
func TestUIUsageRingsWiring(t *testing.T) {
	uiHTML := string(uiHTML)
	for _, want := range []string{
		`id="p-rings"`, `id="d-rings"`, `"five_hour", "5h"`, `"seven_day", "7d"`,
		"renderRings", "setInterval(renderRings, 15000)", "w.resets_at", "w.utilization_pct", "w.state",
		"g.level", "g.held", "g.paused", "g.fail_open", "g.mode", `aria-label="`, "aria-expanded", "USAGE_GLYPH", "USAGE_STATE_WORD",
		"rotate(-90 14 14)", ".uring.RED .arc", ".uring.ORANGE .arc", ".uring.YELLOW .arc", ".uring.GREEN .arc",
	} {
		if !strings.Contains(uiHTML, want) {
			t.Errorf("UI missing %q", want)
		}
	}
	// No client-side thresholds: colour comes from the server's window state.
	for _, bad := range []string{"> 85", ">= 95", "> 1.3", "> 1.6"} {
		if i := strings.Index(uiHTML, "// ---- top-bar usage rings"); i >= 0 && strings.Contains(uiHTML[i:i+9000], bad) {
			t.Errorf("ring code invents threshold %q", bad)
		}
	}
}

// TestUIUsageRingsJS runs the pure ring helpers under node (skipped when
// node is absent; the repo has no other JS runner).
func TestUIUsageRingsJS(t *testing.T) {
	uiHTML := string(uiHTML)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	a := strings.Index(uiHTML, "const USAGE_RING_WINS")
	b := strings.Index(uiHTML, "// renderRings paints")
	if a < 0 || b < a {
		t.Fatal("helper block not found")
	}
	const pre = `
const USAGE_STATES = ["GREEN", "YELLOW", "ORANGE", "RED"], USAGE_STALE_S = 300;
const usageState = (s) => USAGE_STATES.includes(s) ? s : "UNKNOWN";
const esc = (s) => String(s ?? "").replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
function fmtUTC(ts) { return ts ? new Date(ts).toISOString().slice(0, 16).replace("T", " ") + "Z" : "-"; }
`
	const tests = `
const assert = require("assert");
const H = 3600e3, M = 60e3, D = 24*H;
assert.strictEqual(fmtReset(2*H+13*M+5e3), "2h 13m");
assert.strictEqual(fmtReset(6*D+3*H), "6d");
assert.strictEqual(fmtReset(D), "1d");
assert.strictEqual(fmtReset(23*H+59*M), "23h 59m");
assert.strictEqual(fmtReset(H), "1h 0m");
assert.strictEqual(fmtReset(13*M), "13m");
assert.strictEqual(fmtReset(20e3), "<1m");
assert.strictEqual(fmtReset(0), "now");
assert.strictEqual(fmtReset(-5), "now");
assert.strictEqual(fmtResetSpoken(6*D+4*H), "6 days 4 hours");
assert.strictEqual(fmtResetSpoken(2*H+13*M), "2 hours 13 minutes");
assert.strictEqual(fmtResetSpoken(M), "1 minute");
assert.strictEqual(fmtResetSpoken(10e3), "less than a minute");
assert.strictEqual(usageResetMs(null), null);
assert.strictEqual(usageResetMs("garbage"), null);
assert.strictEqual(usageResetMs("2026-01-01T00:00:00Z"), 1767225600000);

const now = Date.parse("2026-11-19T10:00:00Z");
const win = (o) => Object.assign({ utilization_pct: 42, state: "GREEN", age_s: 10, stale: false,
  resets_at: new Date(now + 2*H + 13*M).toISOString(), pace: 0.8 }, o);
// state comes straight from the server (the gate's classifier): 16% can be RED.
let m = usageRingModel(win({ utilization_pct: 16, state: "RED", resets_at: new Date(now + 6*D + 2*H).toISOString() }), "7d", "7-day", now);
assert.strictEqual(m.st, "RED"); assert.strictEqual(m.text1, "7d 16%"); assert.strictEqual(m.text2, "6d");
assert.ok(m.label.includes("status critical (RED)") && m.label.includes("16% used") && m.label.includes("6 days 2 hours"), m.label);
m = usageRingModel(win(), "5h", "5-hour", now);
assert.strictEqual(m.text1, "5h 42%"); assert.strictEqual(m.text2, "2h 13m"); assert.strictEqual(m.st, "GREEN"); assert.strictEqual(m.stale, false);
assert.strictEqual(usageRingModel(win({ state: "YELLOW" }), "5h", "5-hour", now).st, "YELLOW");
assert.strictEqual(usageRingModel(win({ state: "ORANGE" }), "5h", "5-hour", now).st, "ORANGE");
assert.strictEqual(usageRingModel(win({ state: "bogus" }), "5h", "5-hour", now).st, "UNKNOWN");
// stale / missing data degrade, never throw
assert.strictEqual(usageRingModel(win({ stale: true }), "5h", "5-hour", now).stale, true);
assert.strictEqual(usageRingModel(win({ age_s: 999 }), "5h", "5-hour", now).stale, true);
m = usageRingModel(null, "5h", "5-hour", now);
assert.strictEqual(m.st, "UNKNOWN"); assert.strictEqual(m.text1, "5h ?"); assert.strictEqual(m.text2, "—"); assert.ok(m.stale);
m = usageRingModel(win({ utilization_pct: null, resets_at: null }), "5h", "5-hour", now);
assert.strictEqual(m.pct, null); assert.strictEqual(m.text2, "—"); assert.ok(m.label.includes("reset time unknown"));
assert.strictEqual(usageRingModel(win({ utilization_pct: 140 }), "5h", "5-hour", now).pct, 100);
m = usageRingModel(win({ resets_at: new Date(now - M).toISOString() }), "5h", "5-hour", now);
assert.strictEqual(m.text2, "now"); assert.ok(m.label.includes("resetting now"));
// colour is never the only signal: every state has its own glyph and word
for (const st of ["GREEN","YELLOW","ORANGE","RED","UNKNOWN"]) { assert.ok(USAGE_GLYPH[st] && USAGE_STATE_WORD[st]); }
assert.strictEqual(new Set(Object.values(USAGE_GLYPH)).size, 5);
const svg = usageRingSVG(50, "RED"); assert.ok(svg.includes("stroke-dasharray") && svg.includes("✕") && svg.includes('aria-hidden="true"'));
assert.ok(!usageRingSVG(0, "GREEN").includes("stroke-dasharray"));
const btn = usageRingBtn(usageRingModel(win(), "5h", "5-hour", now), "p0");
assert.ok(btn.includes("5h 42%") && btn.includes("2h 13m") && btn.includes('aria-label="') && btn.includes('aria-expanded="false"'));

// hold text mirrors throttle.Rule
assert.ok(usageHoldText({ level: "RED", held: 3 }).includes("RED holds all non-P0 work (3 held)"));
assert.ok(usageHoldText({ level: "ORANGE", held: 0 }).includes("being held"));
assert.ok(usageHoldText({ level: "YELLOW" }).includes("60s"));
assert.strictEqual(usageHoldText({ level: "GREEN" }), "Jobs are not being held.");
assert.ok(usageHoldText({ level: "RED", mode: "off" }).includes("override is ON"));
assert.ok(usageHoldText({ level: "GREEN", paused: true, held: 2 }).includes("PAUSED"));
assert.ok(usageHoldText({ level: "GREEN", fail_open: true }).includes("fails open"));
assert.ok(usageHoldText(null).includes("unavailable"));
// popover carries exact %, reset time, state and hold status
const d = { windows: { five_hour: win({ pace: 1.234 }), seven_day: win({ utilization_pct: 16.25, state: "RED", resets_at: "2026-11-25T00:00:00Z" }) }, gate: { level: "RED", held: 1 } };
const ms = USAGE_RING_WINS.map(([k, s, l]) => usageRingModel(d.windows[k], s, l, now));
const pop = usagePopHTML(d, ms, now);
for (const want of ["16.3% used", "2026-11-25 00:00Z", "status RED (critical)", "pace 1.23x", "Throttle RED", "Jobs ARE being held"]) assert.ok(pop.includes(want), want + "\n" + pop);
`
	dir := t.TempDir()
	f := filepath.Join(dir, "t.js")
	src := pre + uiHTML[a:b] + tests
	if err := os.WriteFile(f, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
		t.Fatalf("syntax: %v\n%s", err, out)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
