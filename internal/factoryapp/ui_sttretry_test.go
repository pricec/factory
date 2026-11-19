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

// sttRetryDriver exercises FactoryVoice.transcribeRetry and the push-to-talk
// recorder's keep-audio-and-retry behaviour (the iPhone "lost what I said" bug).
const sttRetryDriver = `
const assert = require("assert");
const fs = require("fs");
global.window = global;
global.navigator = {};
eval(fs.readFileSync(process.argv[2], "utf8"));
const V = window.FactoryVoice;
const resp = (status, body) => ({ ok: status < 400, status, json: async () => body });
const blob = { type: "audio/mp4", size: 10 };
(async () => {
  // 1. transient 500s are retried with the SAME blob, then succeed.
  let calls = [];
  global.fetch = async (u, o) => { calls.push(o.body); return calls.length < 3 ? resp(500, { detail: "x" }) : resp(200, { text: " hello " }); };
  assert.strictEqual(await V.transcribeRetry(blob, "/s", { delays: [1, 1] }), "hello");
  assert.strictEqual(calls.length, 3);
  assert.ok(calls.every((b) => b === blob), "same audio resent");

  // 2. persistent failure: gives up after 1+2 attempts and hands the blob back.
  calls = [];
  global.fetch = async (u, o) => { calls.push(1); return resp(500, {}); };
  await assert.rejects(V.transcribeRetry(blob, "/s", { delays: [1, 1] }), (e) => e.blob === blob && e.status === 500);
  assert.strictEqual(calls.length, 3);

  // 3. network error is transient too.
  calls = [];
  global.fetch = async () => { calls.push(1); if (calls.length < 2) throw new TypeError("net"); return resp(200, { text: "ok" }); };
  assert.strictEqual(await V.transcribeRetry(blob, "/s", { delays: [1] }), "ok");

  // 4. permanent errors (413, 400) are not retried.
  for (const st of [413, 400]) {
    calls = [];
    global.fetch = async () => { calls.push(1); return resp(st, {}); };
    await assert.rejects(V.transcribeRetry(blob, "/s", { delays: [1, 1] }), (e) => e.status === st);
    assert.strictEqual(calls.length, 1);
  }

  // 5. recorder keeps the failed audio and retry() resends exactly it.
  const errs = [], texts = [];
  let mode = "fail", sent = [];
  global.fetch = async (u, o) => { sent.push(o.body); return mode === "fail" ? resp(500, {}) : resp(200, { text: "again" }); };
  const rec = V.createRecorder({ onText: (t) => texts.push(t), onError: (m, e, k) => errs.push(k) });
  assert.strictEqual(rec.hasFailed(), false);
  assert.strictEqual(typeof rec.retry, "function");
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
`

func TestSTTRetryKeepsAudio(t *testing.T) {
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
	if err := os.WriteFile(drv, []byte(sttRetryDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, drv, lib).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
