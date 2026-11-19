/* Factory service worker. Served at /sw.js (scope /) with the build version
 * substituted into VERSION below, so every deploy changes this file byte-for-byte,
 * the browser installs the new worker, and the old shell cache is dropped.
 *
 * Scope of caching: the app shell only (the page and its static icons/manifest).
 * Everything else - /v1/* API calls, SSE (text/event-stream) and websocket
 * traffic, non-GET requests, other origins - is never touched: no respondWith,
 * the browser goes straight to the network.
 *
 * The page is fetched network-first (so a deploy shows up on the next launch)
 * with a short timeout, falling back to the cached copy so the app still opens
 * instantly when the network is slow or absent.
 */
"use strict";
const VERSION = "__VERSION__";
const CACHE = "factory-shell-" + VERSION;
const SHELL = ["/", "/manifest.webmanifest", "/apple-touch-icon.png", "/icon-192.png", "/icon-512.png", "/icon-maskable-512.png"];
const NAV_TIMEOUT_MS = 2500;

self.addEventListener("install", (e) => {
  e.waitUntil(
    caches.open(CACHE)
      .then((c) => c.addAll(SHELL.map((u) => new Request(u, { cache: "reload" }))))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener("activate", (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k.startsWith("factory-shell-") && k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

function shellRequest(req) {
  if (req.method !== "GET") return false;
  const u = new URL(req.url);
  if (u.origin !== self.location.origin) return false;
  if ((req.headers.get("accept") || "").includes("text/event-stream")) return false;
  if (req.mode === "navigate") return u.pathname === "/";
  return SHELL.includes(u.pathname);
}

self.addEventListener("fetch", (e) => {
  const req = e.request;
  if (!shellRequest(req)) return; // API, SSE, websocket, cross-origin: network only
  const key = new URL(req.url).pathname;
  if (req.mode === "navigate") {
    e.respondWith(
      new Promise((resolve) => {
        let settled = false;
        const fromCache = () => caches.match(key).then((r) => r || fetch(req));
        const timer = setTimeout(() => { if (!settled) { settled = true; fromCache().then(resolve, () => resolve(Response.error())); } }, NAV_TIMEOUT_MS);
        fetch(req, { cache: "no-store" }).then((res) => {
          clearTimeout(timer);
          if (res.ok) { const copy = res.clone(); caches.open(CACHE).then((c) => c.put(key, copy)); }
          if (!settled) { settled = true; resolve(res); }
        }).catch(() => {
          clearTimeout(timer);
          if (!settled) { settled = true; fromCache().then(resolve, () => resolve(Response.error())); }
        });
      })
    );
    return;
  }
  // Static shell assets: cache first (their cache is replaced per version).
  e.respondWith(caches.match(key).then((r) => r || fetch(req)));
});

/* ---- Web Push ---- */

// iOS requires every push to show a notification (userVisibleOnly), so we
// always show one; a tag collapses repeats for the same job/task.
self.addEventListener("push", (e) => {
  let m = {};
  try { m = e.data ? e.data.json() : {}; } catch (_) { m = { body: e.data ? e.data.text() : "" }; }
  e.waitUntil(self.registration.showNotification(m.title || "Factory", {
    body: m.body || "",
    tag: m.tag || undefined,
    renotify: !!m.tag,
    icon: "/icon-192.png",
    badge: "/icon-192.png",
    data: { url: m.url || "/" },
  }));
});

self.addEventListener("notificationclick", (e) => {
  e.notification.close();
  const target = new URL((e.notification.data && e.notification.data.url) || "/", self.location.origin).href;
  e.waitUntil(self.clients.matchAll({ type: "window", includeUncontrolled: true }).then((list) => {
    for (const c of list) {
      if (new URL(c.url).origin === self.location.origin && "focus" in c) {
        return c.focus().then((f) => { if (f && "navigate" in f) return f.navigate(target); });
      }
    }
    return self.clients.openWindow(target);
  }));
});

// The push service may rotate the subscription; tell open pages to resubscribe.
self.addEventListener("pushsubscriptionchange", (e) => {
  e.waitUntil(self.clients.matchAll({ includeUncontrolled: true }).then((list) => list.forEach((c) => c.postMessage({ type: "push-resubscribe" }))));
});
