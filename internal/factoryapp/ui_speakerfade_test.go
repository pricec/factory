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

// speakerFadeDriver checks that decoded TTS chunks start/end at silence (no
// click at chunk edges) and that playback routes through the headroom gain.
const speakerFadeDriver = `
const assert = require("assert");
const fs = require("fs");
global.window = global;
global.navigator = {};
eval(fs.readFileSync(process.argv[2], "utf8"));
const V = window.FactoryVoice;
function buf(rate, len, ch) {
  const data = Array.from({ length: ch }, () => new Float32Array(len).fill(1));
  return { sampleRate: rate, length: len, numberOfChannels: ch, duration: len / rate, getChannelData: (c) => data[c] };
}
// 1. 5 ms at 24 kHz = 120 samples ramped on both edges, middle untouched.
{
  const b = V.fadeEdges(buf(24000, 2400, 2));
  for (let c = 0; c < 2; c++) {
    const d = b.getChannelData(c);
    assert.strictEqual(d[0], 0);
    assert.strictEqual(d[d.length - 1], 0);
    assert.ok(d[60] > 0 && d[60] < 1);
    assert.strictEqual(d[1200], 1);
    assert.ok(d[1] < d[2] && d[d.length - 2] < d[d.length - 3]);
  }
}
// 2. Tiny buffers are faded without overlap or crash.
{
  const d = V.fadeEdges(buf(24000, 10, 1)).getChannelData(0);
  assert.strictEqual(d[0], 0); assert.strictEqual(d[9], 0);
}
// 3. The speaker plays through a master gain below unity, not straight to destination.
(async () => {
  const log = { gain: null, connects: [] };
  const dest = { name: "dest" };
  class Ctx {
    constructor() { this.state = "running"; this.currentTime = 0; this.destination = dest; }
    createBuffer() { return buf(22050, 1, 1); }
    createGain() { const g = { gain: { value: 1 }, connect: (t) => log.connects.push(["gain", t]) }; log.gain = g; return g; }
    createBufferSource() { const s = { connect: (t) => log.connects.push(["src", t]), start() { setTimeout(() => s.onended && s.onended(), 1); }, stop() {}, disconnect() {} }; return s; }
    decodeAudioData(ab, ok) { const b = buf(24000, 2400, 1); ok(b); this.last = b; return Promise.resolve(b); }
  }
  global.AudioContext = Ctx;
  global.fetch = async (u) => ({ ok: true, status: 200, json: async () => ({ chunks: ["a", "b"] }), arrayBuffer: async () => new ArrayBuffer(8) });
  const sp = V.createSpeaker({});
  sp.unlock();
  assert.ok(log.gain.gain.value < 1 && log.gain.gain.value > 0.5);
  assert.ok(log.connects.some((c) => c[0] === "gain" && c[1] === dest));
  log.connects.length = 0; // drop the silent iOS-unlock source
  await sp.speak("hello there");
  const srcTargets = log.connects.filter((c) => c[0] === "src").map((c) => c[1]);
  assert.ok(srcTargets.length >= 2 && srcTargets.every((t) => t === log.gain), "sources feed the master gain");
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
`

func TestSpeakerChunkEdgesFaded(t *testing.T) {
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
	m := regexp.MustCompile(`(?s)<script id="voice-client">(.*?)</script>`).FindSubmatch(b)
	if m == nil {
		t.Fatal("voice-client script block missing")
	}
	dir := t.TempDir()
	lib, drv := filepath.Join(dir, "voice.js"), filepath.Join(dir, "driver.js")
	if err := os.WriteFile(lib, m[1], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(drv, []byte(speakerFadeDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, drv, lib).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
