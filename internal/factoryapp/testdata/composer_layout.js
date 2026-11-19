// Headless-browser check for the phone Planner composer layout (run by
// TestUIPhoneComposerLayoutBrowser; also runnable by hand):
//   NODE_PATH=<dir with playwright> node composer_layout.js <path to ui.html>
// Loads the real ui.html at 390x844 (iPhone UA, standalone, safe-area insets
// stubbed, API mocked) with an iOS-like fake visualViewport, then checks:
//  1. visibility: the last message's bottom edge is above the composer's top
//     edge on load, while scrolling, on focus + keyboard, while the textarea
//     grows/shrinks, when a message arrives, after the keyboard closes;
//  2. no layout feedback loop (the PR #97 regression): layout-var writes,
//     scrollTo calls, <html> class/style mutations and rAF callbacks are
//     counted over 2s of idle (must be zero) and while a storm of synthetic
//     visualViewport resize+scroll events fires (must stay bounded: no
//     padding writes or scrolls when the keyboard height did not change).
//     Like iOS Safari, the fake visualViewport also fires "scroll" whenever
//     the page scrolls, so a measure -> pad -> scroll -> measure cycle shows up.
// Prints "ok" on success; exits 1 listing failures.
const { chromium } = require("playwright");
const fs = require("fs");

const html = fs.readFileSync(process.argv[2], "utf8");
const W = 390, H = 844, SAB = 34, SAT = 47;
const fails = [];

function thread(n) {
  const messages = [];
  for (let i = 0; i < n; i++) {
    const human = i % 2 === 0;
    messages.push({ id: "m" + i, ts: new Date(Date.now() - (n - i) * 60000).toISOString(),
      from: human ? "human:operator" : "planner", type: human ? "chat.message" : "brain.reply",
      payload: { text: (human ? "question " : "answer ") + i + " lorem ipsum dolor sit amet ".repeat(3 + (i % 4) * 4) } });
  }
  return { task: { id: "task_standing", title: "Planner", status: "open", standing: true, priority: 3 }, messages };
}

(async () => {
  const browser = await chromium.launch(process.env.CHROMIUM_PATH ? { executablePath: process.env.CHROMIUM_PATH } : {});
  const ctx = await browser.newContext({ viewport: { width: W, height: H }, deviceScaleFactor: 3, hasTouch: true, isMobile: true,
    userAgent: "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1" });
  const page = await ctx.newPage();
  page.on("pageerror", (e) => fails.push("pageerror: " + e.message));
  let msgs = thread(14);
  await page.route("http://factory.test/**", async (route) => {
    const u = new URL(route.request().url());
    const json = (b) => route.fulfill({ contentType: "application/json", body: JSON.stringify(b) });
    if (u.pathname === "/") return route.fulfill({ contentType: "text/html", body: html });
    if (u.pathname === "/v1/tasks/task_standing") return json(msgs);
    if (u.pathname === "/v1/tasks") return json({ tasks: [] });
    if (u.pathname === "/v1/alerts") return json({ alerts: [] });
    if (u.pathname === "/v1/usage") return json({});
    return route.fulfill({ status: 404, contentType: "application/json", body: "{}" });
  });
  await page.addInitScript(([SAB, SAT]) => {
    Object.defineProperty(navigator, "standalone", { value: true });
    // iOS-like visualViewport: the layout viewport (innerHeight) never shrinks for the keyboard; vv.height does.
    const vv = new EventTarget();
    Object.assign(vv, { width: innerWidth, height: innerHeight, offsetTop: 0, offsetLeft: 0, pageTop: 0, pageLeft: 0, scale: 1 });
    Object.defineProperty(window, "visualViewport", { value: vv });
    let kb = 0;
    window.__setKb = (k) => { kb = k; vv.height = innerHeight - kb; vv.dispatchEvent(new Event("resize")); };
    window.__vvFire = (type, patch) => { Object.assign(vv, patch || {}); vv.dispatchEvent(new Event(type)); };
    // iOS fires visualViewport "scroll" while the page scrolls
    window.addEventListener("scroll", () => { vv.pageTop = scrollY; vv.dispatchEvent(new Event("scroll")); }, { passive: true });
    window.addEventListener("resize", () => { vv.width = innerWidth; vv.height = innerHeight - kb; });
    // counters for the loop check
    const C = window.__c = { vars: {}, scrollTo: 0, raf: 0, htmlMut: 0 };
    const sp = CSSStyleDeclaration.prototype.setProperty;
    CSSStyleDeclaration.prototype.setProperty = function (k, v, p) {
      if (this === document.documentElement.style) C.vars[k] = (C.vars[k] || 0) + 1;
      return sp.call(this, k, v, p);
    };
    const st = window.scrollTo.bind(window);
    window.scrollTo = function (...a) { C.scrollTo++; return st(...a); };
    const raf = window.requestAnimationFrame.bind(window);
    window.requestAnimationFrame = (cb) => raf((t) => { C.raf++; cb(t); });
    window.__reset = () => { C.vars = {}; C.scrollTo = 0; C.raf = 0; C.htmlMut = 0; };
    document.addEventListener("DOMContentLoaded", () => {
      new MutationObserver((l) => { C.htmlMut += l.length; }).observe(document.documentElement, { attributes: true, attributeFilter: ["class", "style"] });
      const s = document.createElement("style");
      s.textContent = "html.phone{--sab:" + SAB + "px !important;--sat:" + SAT + "px !important}";
      document.head.appendChild(s);
    });
  }, [SAB, SAT]);
  await page.goto("http://factory.test/#/planner");
  await page.waitForSelector("#p-body .bubble");
  await page.waitForTimeout(600);

  const sleep = (ms) => page.waitForTimeout(ms);
  // gap = composer top - last bubble bottom (must be >= 0)
  const measure = () => page.evaluate(() => {
    const b = [...document.querySelectorAll("#p-body .bubble")].pop().getBoundingClientRect();
    const c = document.getElementById("p-composer").getBoundingClientRect();
    return { gap: Math.round((c.top - b.bottom) * 10) / 10, bubbleBottom: b.bottom, composerTop: c.top, scrollY: scrollY,
      max: document.documentElement.scrollHeight - innerHeight, kb: document.documentElement.classList.contains("kb-open") };
  });
  const visible = async (name) => { const m = await measure(); if (m.gap < 0) fails.push(name + ": last message under the composer " + JSON.stringify(m)); return m; };
  const toBottom = async () => { await page.evaluate(() => scrollTo(0, document.documentElement.scrollHeight)); await sleep(150); };
  const counters = () => page.evaluate(() => JSON.parse(JSON.stringify(window.__c)));
  const LAYOUT_VARS = ["--composer-h", "--kbpad", "--cmp-inset"];
  const padWrites = (c) => LAYOUT_VARS.reduce((n, k) => n + (c.vars[k] || 0), 0);
  // idle: nothing may write layout, scroll, or schedule frames
  const idle = async (name) => {
    await sleep(400); await page.evaluate(() => __reset()); await sleep(2000);
    const c = await counters();
    if (padWrites(c) || c.scrollTo || c.htmlMut || Object.keys(c.vars).length || c.raf > 4)
      fails.push(name + ": not idle after settling " + JSON.stringify(c));
  };
  // ~2s of iOS-style visualViewport noise (resize+scroll at 60Hz) with the given per-tick geometry
  const storm = async (name, tick, max) => {
    await page.evaluate(() => __reset());
    await page.evaluate(async (tickSrc) => {
      const tick = eval(tickSrc);
      for (let i = 0; i < 120; i++) {
        const p = tick(i);
        __vvFire("resize", p); __vvFire("scroll", p);
        await new Promise((r) => setTimeout(r, 16));
      }
    }, tick.toString());
    await sleep(300);
    const c = await counters();
    const bad = padWrites(c) > max.pad || c.scrollTo > max.scroll || c.htmlMut > max.mut || c.raf > max.raf;
    if (bad) fails.push(name + ": layout work not bounded during visualViewport storm " + JSON.stringify(c) + " limits " + JSON.stringify(max));
    return c;
  };

  // 1. load + scrolling
  await visible("initial load");
  await toBottom(); await visible("scrolled to bottom");
  for (const y of [0, 300, 900, 100000]) { await page.evaluate((y) => scrollTo(0, y), y); await sleep(60); }
  await visible("scroll round trip");
  await idle("idle after load");

  // 2. visualViewport noise while nothing changes (toolbar settle, page scroll forwarding) and toolbar jiggle < keyboard threshold
  await storm("vv noise, unchanged geometry", () => ({}), { pad: 0, scroll: 0, mut: 0, raf: 4 });
  await storm("vv toolbar jiggle", (i) => ({ height: innerHeight - (i % 2 ? 44 : 0) }), { pad: 0, scroll: 0, mut: 130, raf: 130 });
  await page.evaluate(() => __vvFire("resize", { height: innerHeight, offsetTop: 0 }));
  await toBottom(); await visible("after toolbar jiggle");
  await idle("idle after vv noise");

  // 3. focus + keyboard, typing grows the box, then shrinks
  await page.evaluate(() => __reset());
  await page.click("#p-input");
  await page.evaluate(() => __setKb(336)); await sleep(600);
  await visible("focus + keyboard open");
  let c = await counters();
  if (padWrites(c) > 6 || c.scrollTo > 4) fails.push("keyboard open: too much layout work " + JSON.stringify(c));
  await idle("idle with keyboard open");
  await storm("vv noise, keyboard open", () => ({}), { pad: 0, scroll: 0, mut: 0, raf: 4 });
  // panning the visual viewport with the keyboard up moves the composer (transform) but must never re-pad or scroll
  await storm("vv pan, keyboard open", (i) => ({ offsetTop: (i % 30) * 4 }), { pad: 0, scroll: 0, mut: 130, raf: 130 });
  await page.evaluate(() => __vvFire("scroll", { offsetTop: 0 })); await sleep(100);
  const line = "typing a longer message that wraps ";
  for (let i = 1; i <= 8; i++) {
    await page.fill("#p-input", Array(i).fill(line + i).join("\n")); await sleep(150);
    await visible("textarea grows (" + i + " lines)");
  }
  await page.fill("#p-input", "x"); await sleep(200);
  await visible("textarea shrinks");
  await idle("idle after typing");
  await page.evaluate(() => __setKb(0)); await page.evaluate(() => document.activeElement.blur()); await sleep(500);
  await toBottom(); await visible("keyboard closed");

  // 4. keyboard opens while at the bottom: stay at the bottom
  await page.click("#p-input");
  await page.evaluate(() => __setKb(336)); await sleep(600);
  const k = await visible("keyboard open at bottom");
  if (k.max - k.scrollY > 4) fails.push("keyboard open: was at bottom but left " + (k.max - k.scrollY) + "px above it");
  await page.evaluate(() => __setKb(0)); await page.evaluate(() => document.activeElement.blur()); await sleep(400);

  // 5. reader scrolled up: growing the box must NOT move them
  await page.evaluate(() => scrollTo(0, 200)); await sleep(150);
  const y0 = await page.evaluate(() => scrollY);
  await page.evaluate(() => __reset());
  await page.fill("#p-input", Array(6).fill(line).join("\n")); await sleep(300);
  const y1 = await page.evaluate(() => scrollY);
  c = await counters();
  if (Math.abs(y1 - y0) > 2 || c.scrollTo) fails.push("scrolled-up reader was moved: " + y0 + " -> " + y1 + " " + JSON.stringify(c));
  await page.fill("#p-input", ""); await sleep(200);
  await toBottom(); await visible("box shrank back");

  // 6. new message arrives while at the bottom
  msgs = thread(15);
  await page.evaluate(() => pLoad()); await sleep(500);
  await visible("new message arrives");
  await idle("idle at end");

  // 7. the padding tracks the real composer height
  const v = await page.evaluate(() => ({ css: parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--composer-h")), real: document.getElementById("p-composer").offsetHeight }));
  if (Math.abs(v.css - v.real) > 1) fails.push("--composer-h (" + v.css + ") != composer height (" + v.real + ")");

  await browser.close();
  if (fails.length) { console.log(fails.join("\n")); process.exit(1); }
  console.log("ok");
})().catch((e) => { console.log(e.stack || e); process.exit(1); });
