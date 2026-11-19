package factoryapp_test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"testing"
)

// handsFreeDriver exercises window.FactoryTurn (the pure VAD turn state
// machine from ui.html's #voice-turn block) with synthetic RMS frames at 50 ms.
const handsFreeDriver = `
const assert = require("assert");
const fs = require("fs");
global.window = {};
eval(fs.readFileSync(process.argv[2], "utf8"));
const T = window.FactoryTurn;
// feed(m, from, to, rms, playing): frames every 50 ms in [from,to); returns events
function feed(m, from, to, rms, playing) {
  const ev = [];
  for (let t = from; t < to; t += 50) for (const e of m.step(rms, t, !!playing)) ev.push(Object.assign({ at: t }, e));
  return ev;
}
const types = (ev) => ev.map((e) => e.type);

// 1. A normal utterance: speech, then 1.3 s of silence ends it exactly once.
{
  const m = T.create();
  let ev = feed(m, 0, 2000, 0.003);
  assert.deepStrictEqual(ev, []);
  ev = feed(m, 2000, 3000, 0.1);
  assert.deepStrictEqual(types(ev), ["speech_start"]);
  assert.strictEqual(ev[0].bargeIn, false);
  assert.strictEqual(m.phase(), "speech");
  ev = feed(m, 3000, 4000, 0.002);          // 1 s quiet: not yet
  assert.deepStrictEqual(ev, []);
  ev = feed(m, 4000, 4600, 0.002);
  assert.deepStrictEqual(types(ev), ["utterance_end"]);
  assert.ok(ev[0].at - 2950 >= 1300 && ev[0].at - 2950 < 1500, "ends ~silenceMs after last voice: " + ev[0].at);
  assert.strictEqual(m.phase(), "listening");
  // and it re-arms: a second utterance works
  ev = feed(m, 5000, 5600, 0.1).concat(feed(m, 5600, 7000, 0));
  assert.deepStrictEqual(types(ev), ["speech_start", "utterance_end"]);
}
// 2. Short pauses inside an utterance do not end it.
{
  const m = T.create();
  feed(m, 0, 500, 0.1);
  assert.deepStrictEqual(feed(m, 500, 1500, 0), []);
  assert.deepStrictEqual(feed(m, 1500, 2000, 0.1), []);
  assert.strictEqual(m.phase(), "speech");
}
// 3. Blips (< 300 ms of voice) are discarded, not sent.
{
  const m = T.create();
  const ev = feed(m, 0, 200, 0.1).concat(feed(m, 200, 3000, 0));
  assert.deepStrictEqual(types(ev), ["speech_start", "blip"]);
}
// 3b. A click shorter than the onset window never starts speech.
{
  const m = T.create();
  assert.deepStrictEqual(feed(m, 0, 100, 0.2).concat(feed(m, 100, 3000, 0)), []);
}
// 4. Raised threshold during playback: echo-level energy is ignored...
{
  const m = T.create();
  const echo = m.threshold(false) * 1.5;      // above the idle threshold, below the playback one
  assert.ok(echo < m.threshold(true));
  assert.deepStrictEqual(feed(m, 0, 3000, echo, true), []);
  // ...but the same energy IS speech when nothing is playing
  const m2 = T.create();
  assert.deepStrictEqual(types(feed(m2, 0, 500, echo, false)), ["speech_start"]);
}
// 5. Barge-in: loud speech during playback reports bargeIn:true.
{
  const m = T.create();
  const ev = feed(m, 0, 1000, 0.3, true);
  assert.deepStrictEqual(types(ev), ["speech_start"]);
  assert.strictEqual(ev[0].bargeIn, true);
}
// 6. Adaptive noise floor: a noisy room raises the threshold; steady noise is not speech.
{
  const m = T.create();
  const quiet = m.threshold(false);
  feed(m, 0, 5000, 0.015);                    // below minThr, but raises the floor
  assert.ok(m.threshold(false) > quiet);
  assert.deepStrictEqual(feed(m, 5000, 8000, 0.015), []);
  assert.ok(m.floor() <= m.config.floorMax);
}
// 7. Hard cap ends a never-ending utterance.
{
  const m = T.create({ maxUtteranceMs: 5000 });
  const ev = feed(m, 0, 5200, 0.2);
  assert.deepStrictEqual(types(ev), ["speech_start", "utterance_end"]);
}
// 8. silenceMs is tunable.
{
  const m = T.create({ silenceMs: 1200 });
  const ev = feed(m, 0, 1000, 0.1).concat(feed(m, 1000, 3000, 0));
  assert.deepStrictEqual(types(ev), ["speech_start", "utterance_end"]);
  assert.ok(ev[1].at < 950 + 1300);
}
console.log("ok");
`

// TestHandsFreeTurnMachine runs the pure FactoryTurn state machine from the
// served page under node. Skipped when node is not installed.
func TestHandsFreeTurnMachine(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	e := newEnv(t)
	resp, err := http.Get(e.ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	m := regexp.MustCompile(`(?s)<script id="voice-turn">(.*?)</script>`).FindSubmatch(b)
	if m == nil {
		t.Fatal("voice-turn script block missing")
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "turn.js")
	drv := filepath.Join(dir, "driver.js")
	if err := os.WriteFile(lib, m[1], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(drv, []byte(handsFreeDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, drv, lib).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
