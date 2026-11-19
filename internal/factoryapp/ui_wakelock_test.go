package factoryapp_test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// wakeDriver exercises window.FactoryWake (the hands-free screen keep-awake
// from ui.html's #voice-wake block) against mock navigator/document objects.
const wakeDriver = `
const assert = require("assert");
const fs = require("fs");
global.window = {};
eval(fs.readFileSync(process.argv[2], "utf8"));
const W = window.FactoryWake;
const tick = () => new Promise((r) => setImmediate(r));

function mkDoc() {
  const l = {};
  const d = {
    hidden: false, visibilityState: "visible", body: { kids: [], appendChild(v) { v.parentNode = this; this.kids.push(v); }, removeChild(v) { this.kids = this.kids.filter((x) => x !== v); v.parentNode = null; } },
    vids: [],
    addEventListener(n, f) { (l[n] = l[n] || []).push(f); },
    removeEventListener(n, f) { l[n] = (l[n] || []).filter((x) => x !== f); },
    fire(n) { (l[n] || []).slice().forEach((f) => f()); },
    listeners: (n) => (l[n] || []).length,
    createElement() {
      const a = {};
      const v = { style: {}, attrs: a, plays: 0, paused: true, setAttribute(k, x) { a[k] = x; }, removeAttribute(k) { delete a[k]; },
        addEventListener() {}, load() {}, pause() { this.paused = true; },
        play() { this.plays++; this.paused = false; return Promise.resolve(); } };
      d.vids.push(v); return v;
    },
  };
  return d;
}
function mkNav(opts) {
  const n = { requests: [], sentinels: [], failNext: opts && opts.fail };
  n.wakeLock = { request(type) {
    n.requests.push(type);
    if (n.failNext) return Promise.reject(new Error("NotAllowedError"));
    const s = { released: false, ls: [], addEventListener(ev, f) { this.ls.push(f); }, release() { this.released = true; this.ls.forEach((f) => f()); } };
    n.sentinels.push(s); return Promise.resolve(s);
  } };
  return n;
}

(async () => {
  // 1. Native API: request("screen") on acquire, release on release, no video.
  {
    const nav = mkNav(), doc = mkDoc(), w = W.create({ nav, doc });
    w.acquire(); await tick();
    assert.deepStrictEqual(nav.requests, ["screen"]);
    assert.strictEqual(w.mode(), "wakelock");
    assert.strictEqual(doc.vids.length, 0);
    w.release();
    assert.strictEqual(nav.sentinels[0].released, true);
    assert.strictEqual(w.mode(), "off");
    assert.strictEqual(doc.listeners("visibilitychange"), 0, "listener removed on release");
  }
  // 2. UA releases the lock (tab hidden): re-acquired when visible again, not while hidden.
  {
    const nav = mkNav(), doc = mkDoc(), w = W.create({ nav, doc });
    w.acquire(); await tick();
    doc.hidden = true; doc.visibilityState = "hidden";
    nav.sentinels[0].release(); await tick();            // UA-initiated release while hidden
    assert.strictEqual(nav.requests.length, 1, "no request while hidden");
    assert.strictEqual(w.mode(), "off");
    doc.hidden = false; doc.visibilityState = "visible";
    doc.fire("visibilitychange"); await tick();
    assert.strictEqual(nav.requests.length, 2);
    assert.strictEqual(w.mode(), "wakelock");
    // released while visible (e.g. low power): re-requested straight away
    nav.sentinels[1].release(); await tick();
    assert.strictEqual(nav.requests.length, 3);
    assert.strictEqual(w.mode(), "wakelock");
    w.release();
    nav.sentinels[2].release(); await tick();            // late release event after we turned off
    assert.strictEqual(nav.requests.length, 3, "no re-acquire after release()");
  }
  // 3. release() while the request is still in flight does not leak a lock.
  {
    const nav = mkNav(), doc = mkDoc(), w = W.create({ nav, doc });
    w.acquire(); w.release(); await tick();
    assert.strictEqual(nav.sentinels[0].released, true);
    assert.strictEqual(w.mode(), "off");
  }
  // 4. No Wake Lock API: muted inline looping video, started synchronously (inside the tap).
  {
    const doc = mkDoc(), w = W.create({ nav: {}, doc });
    w.acquire();
    assert.strictEqual(doc.vids.length, 1);
    const v = doc.vids[0];
    assert.strictEqual(v.plays, 1, "play() called synchronously in acquire()");
    assert.strictEqual(v.muted, true); assert.strictEqual(v.loop, true);
    assert.ok("playsinline" in v.attrs && "muted" in v.attrs);
    assert.ok(/^data:video\/mp4;base64,/.test(v.src));
    assert.strictEqual(w.mode(), "video");
    // iOS pauses it in the background: ensure()/visibilitychange restarts it
    v.paused = true; doc.fire("visibilitychange");
    assert.strictEqual(v.plays, 2);
    w.release();
    assert.strictEqual(v.paused, true); assert.strictEqual(doc.body.kids.length, 0);
    assert.strictEqual(w.mode(), "off");
  }
  // 5. API present but refused: falls back to video; later success supersedes it.
  {
    const nav = mkNav({ fail: true }), doc = mkDoc(), w = W.create({ nav, doc });
    w.acquire(); await tick();
    assert.strictEqual(w.mode(), "video");
    nav.failNext = false; doc.fire("visibilitychange"); await tick();
    assert.strictEqual(w.mode(), "wakelock");
    assert.strictEqual(doc.body.kids.length, 0, "video removed once the real lock is held");
    w.release();
  }
  // 6. Watchdog: a silently dropped lock is re-acquired by the timer alone (no gesture, no event).
  {
    const nav = mkNav(), doc = mkDoc(), w = W.create({ nav, doc, watchMs: 5 });
    w.acquire(); await tick();
    w.ensure(); assert.strictEqual(nav.requests.length, 1, "ensure() is a no-op while held");
    w.release();
  }
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
`

// TestHandsFreeWakeLock runs FactoryWake from the served page under node and
// checks that hands-free wires it in. Skipped when node is not installed.
func TestHandsFreeWakeLock(t *testing.T) {
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
	m := regexp.MustCompile(`(?s)<script id="voice-wake">(.*?)</script>`).FindSubmatch(b)
	if m == nil {
		t.Fatal("voice-wake script block missing")
	}
	// Wiring: acquired in start() before any await, released in stop().
	body := string(b)
	for _, want := range []string{"window.FactoryWake.create()", "wake.acquire()", "wake.release()", `wakeLock.request("screen")`} {
		if !strings.Contains(string(m[1])+body, want) {
			t.Errorf("UI page missing wake-lock hook %q", want)
		}
	}
	if strings.Index(body, "wake.acquire()") > strings.Index(body, "await navigator.mediaDevices.getUserMedia({ audio: { channelCount: 1, echoCancellation: true, noiseSuppression: true, autoGainControl") {
		t.Error("wake.acquire() must run before the getUserMedia await (user-gesture window)")
	}
	dir := t.TempDir()
	lib := filepath.Join(dir, "wake.js")
	drv := filepath.Join(dir, "driver.js")
	if err := os.WriteFile(lib, m[1], 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(drv, []byte(wakeDriver), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(node, drv, lib).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}
