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

func getBody(t *testing.T, url string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

// TestServiceWorkerServed: /sw.js is never HTTP-cached, carries a concrete
// version (cache busting), and is stable between requests of one build.
func TestServiceWorkerServed(t *testing.T) {
	e := newEnv(t)
	resp, body := getBody(t, e.ts.URL+"/sw.js")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/javascript") {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q, want no-cache", cc)
	}
	if strings.Contains(body, "__VERSION__") || !regexp.MustCompile(`const VERSION = "[^"]*-[0-9a-f]{12}";`).MatchString(body) {
		t.Fatalf("version not substituted:\n%.300s", body)
	}
	if _, again := getBody(t, e.ts.URL+"/sw.js"); again != body {
		t.Fatal("sw.js differs between requests of the same build")
	}
}

// TestPWAShellHooks pins the page's PWA wiring.
func TestPWAShellHooks(t *testing.T) {
	e := newEnv(t)
	_, body := getBody(t, e.ts.URL+"/")
	for _, want := range []string{
		`register("/sw.js"`, `id="push-client"`, `class="p-card push-panel"`, `data-push="enable"`, `data-push="disable"`,
		`content="Factory"`, "viewport-fit=cover", "display-mode: standalone",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
}

const swDriver = `
const assert = require("assert");
const fs = require("fs");
const src = fs.readFileSync(process.argv[2], "utf8");
const handlers = {}, store = {};
const caches = {
  keys: async () => Object.keys(store),
  delete: async (k) => { delete store[k]; return true; },
  open: async (k) => (store[k] = store[k] || { added: [], addAll: async function (r) { this.added.push(...r.map((x) => new URL(x.url).pathname)); }, put: async () => {} }),
  match: async () => undefined,
};
const shown = [];
const self = {
  location: new URL("https://factory.test/sw.js"),
  addEventListener: (n, f) => { handlers[n] = f; },
  skipWaiting: () => Promise.resolve(), clients: { claim: async () => {}, matchAll: async () => [], openWindow: async () => {} },
  registration: { showNotification: async (t, o) => shown.push([t, o]) },
};
global.Request = class { constructor(u, o) { this.url = new URL(u, "https://factory.test").href; } };
new Function("self", "caches", "Request", src)(self, caches, global.Request);

function fetchEv(url, { method = "GET", mode = "cors", accept = "*/*" } = {}) {
  let responded = false;
  return { ev: { request: { url, method, mode, headers: { get: (h) => (h.toLowerCase() === "accept" ? accept : null) } }, respondWith() { responded = true; } }, handled: () => responded };
}
(async () => {
  // install precaches the shell
  let p; handlers.install({ waitUntil: (x) => (p = x) }); await p;
  const cache = store["factory-shell-TESTVER"];
  assert.ok(cache, "versioned cache created");
  assert.ok(cache.added.includes("/") && cache.added.includes("/icon-192.png"), "shell precached: " + cache.added);
  // activate drops old versions only
  store["factory-shell-old"] = {}; store["unrelated"] = {};
  handlers.activate({ waitUntil: (x) => (p = x) }); await p;
  assert.ok(!store["factory-shell-old"] && store["unrelated"] && store["factory-shell-TESTVER"], Object.keys(store));
  // never intercepted: API, SSE, websocket upgrade paths, non-GET, cross-origin, other pages
  for (const [url, o] of [
    ["https://factory.test/v1/jobs"], ["https://factory.test/v1/usage"], ["https://factory.test/v1/throttle"], ["https://factory.test/v1/usage", { mode: "navigate" }], ["https://factory.test/v1/inboxes/planner:main/stream", { accept: "text/event-stream" }],
    ["https://factory.test/v1/chat", { method: "POST" }], ["https://factory.test/", { method: "POST" }],
    ["https://evil.test/", { mode: "navigate" }], ["https://factory.test/anything-else"], ["https://factory.test/v1/push/key"],
    ["wss://factory.test/v1/ws"], ["https://factory.test/v1/x", { mode: "navigate" }],
  ]) { const f = fetchEv(url, o); handlers.fetch(f.ev); assert.ok(!f.handled(), "must not intercept " + url); }
  // shell: intercepted
  for (const [url, o] of [["https://factory.test/", { mode: "navigate" }], ["https://factory.test/icon-512.png"], ["https://factory.test/manifest.webmanifest"]]) {
    global.fetch = async () => ({ ok: true, clone() { return this; } });
    const f = fetchEv(url, o); handlers.fetch(f.ev); assert.ok(f.handled(), "should serve shell " + url);
  }
  // push always shows a notification (iOS revokes subscriptions that don't)
  handlers.push({ data: { json: () => ({ title: "Job J1 failed", body: "boom", url: "/#/inbox", tag: "job-J1" }) }, waitUntil: (x) => (p = x) }); await p;
  handlers.push({ data: null, waitUntil: (x) => (p = x) }); await p;
  assert.strictEqual(shown.length, 2); assert.strictEqual(shown[0][0], "Job J1 failed"); assert.strictEqual(shown[1][0], "Factory");
  assert.strictEqual(shown[0][1].data.url, "/#/inbox");
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
`

const pushDriver = `
const assert = require("assert");
const fs = require("fs");
global.window = { atob: (s) => Buffer.from(s, "base64").toString("binary") };
eval(fs.readFileSync(process.argv[2], "utf8"));
const FP = window.FactoryPush;
const ua = { iphoneSafari: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X)" };

function env({ perm = "default", push = true, standalone = true, sub = null, keyStatus = 200, ua: agent = ua.iphoneSafari } = {}) {
  const calls = [], ls = {}; let cur = sub;
  const pm = {
    getSubscription: async () => cur,
    subscribe: async (o) => { calls.push(["subscribe", o]); cur = { endpoint: "https://web.push.apple.com/z", toJSON() { return { endpoint: this.endpoint, keys: { p256dh: "a", auth: "b" } }; }, unsubscribe: async () => { cur = null; } }; return cur; },
  };
  const N = { permission: perm, requestPermission: async () => { calls.push(["perm"]); if (perm === "default") N.permission = "granted"; return N.permission; } };
  const w = { PushManager: push ? {} : undefined, atob: global.window.atob, localStorage: { setItem: (k, v) => (ls[k] = v), removeItem: (k) => delete ls[k], getItem: (k) => ls[k] }, matchMedia: () => ({ matches: false }) };
  const nav = { userAgent: agent, platform: "iPhone", maxTouchPoints: 5, standalone, serviceWorker: { ready: Promise.resolve({ pushManager: pm }) } };
  const fetch = async (u, o) => { calls.push(["fetch", u, o && o.method]); if (u === "/v1/push/key") return { ok: keyStatus === 200, status: keyStatus, json: async () => ({ public_key: "BAAA" }) }; return { ok: true, status: 200 }; };
  return { calls, ls, api: FP.create({ navigator: nav, window: w, Notification: N, fetch }) };
}
(async () => {
  // plain iOS Safari (no PushManager): tell the user to install, not "unsupported"
  assert.strictEqual((await env({ push: false, standalone: false }).api.status()).state, "install");
  assert.strictEqual((await env({ push: false, standalone: false, ua: "Mozilla/5.0 (X11; Linux)" }).api.status()).state, "unsupported");
  assert.strictEqual((await env({ perm: "denied" }).api.status()).state, "denied");
  // enable: permission first (user gesture), then key, subscribe userVisibleOnly, register with server
  const e = env();
  assert.strictEqual((await e.api.status()).state, "off");
  await e.api.enable();
  assert.deepStrictEqual(e.calls.map((c) => c[0] + (c[1] && typeof c[1] === "string" ? ":" + c[1] : "")), ["perm", "fetch:/v1/push/key", "subscribe", "fetch:/v1/push/subscribe"]);
  assert.strictEqual(e.calls[2][1].userVisibleOnly, true);
  assert.ok(e.calls[2][1].applicationServerKey instanceof Uint8Array);
  assert.strictEqual(e.ls["factory.push"], "1");
  assert.strictEqual((await e.api.status()).state, "on");
  // disable: server told, subscription dropped
  await e.api.disable();
  assert.ok(e.calls.some((c) => c[1] === "/v1/push/unsubscribe" && c[2] === "POST"));
  assert.strictEqual((await e.api.status()).state, "off"); assert.strictEqual(e.ls["factory.push"], undefined);
  // permission refused -> error, nothing subscribed
  await assert.rejects(env({ perm: "denied" }).api.enable(), /not allowed/);
  // server push disabled -> clear error
  await assert.rejects(env({ keyStatus: 404 }).api.enable(), /disabled/);
  console.log("ok");
})().catch((e) => { console.error(e); process.exit(1); });
`

func runNode(t *testing.T, driver string, libName string, lib string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	libPath, drvPath := filepath.Join(dir, libName), filepath.Join(dir, "driver.js")
	if err := os.WriteFile(libPath, []byte(lib), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(drvPath, []byte(driver), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, drvPath, libPath).CombinedOutput(); err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
}

// TestServiceWorkerBehaviour runs the served /sw.js under node with mocks:
// only the shell is cached; API/SSE/non-GET/cross-origin traffic is never
// intercepted; old caches are dropped; pushes always show a notification.
func TestServiceWorkerBehaviour(t *testing.T) {
	e := newEnv(t)
	_, body := getBody(t, e.ts.URL+"/sw.js")
	body = regexp.MustCompile(`const VERSION = "[^"]*";`).ReplaceAllString(body, `const VERSION = "TESTVER";`)
	runNode(t, swDriver, "sw.js", body)
}

// TestFactoryPushClient runs the page's FactoryPush logic under node.
func TestFactoryPushClient(t *testing.T) {
	e := newEnv(t)
	_, page := getBody(t, e.ts.URL+"/")
	m := regexp.MustCompile(`(?s)<script id="push-client">(.*?)</script>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("push-client script missing")
	}
	runNode(t, pushDriver, "push.js", m[1])
}

// TestStandaloneHeaderNav pins the Home Screen app navigation: with no browser
// chrome, a header nav (#p-hnav) links every top-level phone page on the phone
// routes where the bottom tab bar is hidden (thread, focus, keyboard up), gated
// on html.standalone and inset by the safe areas. Where the bottom bar is shown
// it stays the only page nav, so the header strip is hidden and its height
// dropped. The desktop header and the bottom tab bar stay.
func TestStandaloneHeaderNav(t *testing.T) {
	e := newEnv(t)
	_, body := getBody(t, e.ts.URL+"/")
	for _, want := range []string{
		`navigator.standalone`, `classList.add("standalone")`,
		`<nav id="p-hnav"`,
		`html.phone.standalone #p-hnav { display: flex;`,
		`#p-hnav { display: none; }`,
		`html.phone.standalone { --topx:`,
		`html.phone.standalone:not(.p-thread):not(.kb-open) { --topx: 0px; }`,
		`html.phone.standalone:not(.p-thread):not(.kb-open) #p-hnav { display: none; }`,
		`var(--sar)`, `var(--sal)`,
		`top: calc(52px + var(--sat) + var(--topx))`,
		`#p-tabs button, #p-hnav button`,
		`<nav id="p-tabs">`, `<button id="tab-planner"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	nav := body[strings.Index(body, `<nav id="p-hnav"`):]
	nav = nav[:strings.Index(nav, "</nav>")]
	for _, tab := range []string{"planner", "inbox", "alerts", "jobs", "foremen", "more"} {
		if !strings.Contains(nav, `data-tab="`+tab+`"`) {
			t.Errorf("#p-hnav missing tab %q", tab)
		}
	}
	// the nav is only ever shown by the standalone+phone rule
	if strings.Contains(body, "\n  #p-hnav { display: flex") || strings.Count(body, "#p-hnav { display: none; }") != 2 {
		t.Error("#p-hnav must be hidden by default and shown only under html.phone.standalone")
	}
}

// TestBottomTabBarHasEverySection: the bottom tab bar (#p-tabs) is the only
// page nav where the header strip is hidden, so it must link every top-level
// phone page that the router knows (P_TAB keys that are tabs).
func TestBottomTabBarHasEverySection(t *testing.T) {
	e := newEnv(t)
	_, body := getBody(t, e.ts.URL+"/")
	bar := body[strings.Index(body, `<nav id="p-tabs">`):]
	bar = bar[:strings.Index(bar, "</nav>")]
	for _, tab := range []string{"planner", "inbox", "alerts", "jobs", "foremen", "more"} {
		if !strings.Contains(bar, `data-tab="`+tab+`"`) {
			t.Errorf("#p-tabs missing section %q", tab)
		}
	}
	if !strings.Contains(body, `const P_TAB = { planner: "planner", alerts: "alerts", inbox: "inbox"`) {
		t.Error("router P_TAB no longer declares the tab set this test checks")
	}
}

// TestTasksFocusKeepsBottomNav: the Tasks focus thread (html.p-fthread) must
// behave like the other pages on a phone: the bottom tab bar stays (it is
// never hidden by p-fthread), the standalone header strip is hidden, and the
// composer / prev-skip-next bar stack above the tab bar.
func TestTasksFocusKeepsBottomNav(t *testing.T) {
	e := newEnv(t)
	_, body := getBody(t, e.ts.URL+"/")
	for _, bad := range []string{`html.p-fthread #p-tabs`, `:not(.p-fthread)`} {
		if strings.Contains(body, bad) {
			t.Errorf("UI still special-cases p-fthread via %q: Tasks would lose the bottom nav / show the top nav", bad)
		}
	}
	for _, want := range []string{
		`html.p-fthread #p-composer { bottom: calc(var(--tabh) + var(--sab)); }`,
		`html.p-fthread.kb-open #p-composer { bottom: 0; }`,
		`bottom: calc(var(--tabh) + var(--sab) + var(--composer-h) + var(--kb));`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("UI missing %q", want)
		}
	}
}
