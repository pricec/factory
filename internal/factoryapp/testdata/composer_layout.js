// Headless-browser check for the phone Planner composer layout (run by
// TestUIPhoneComposerLayoutBrowser; also runnable by hand):
//   NODE_PATH=<dir with playwright> node composer_layout.js <path to ui.html>
// Loads the real ui.html at 390x844 with the API mocked, standalone mode and
// safe-area insets stubbed, and a fake visualViewport for the iOS keyboard,
// then asserts the last message's bottom edge is above the composer's top
// edge in every state. Prints "ok" on success; exits 1 listing failures.
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
  // standalone PWA + iOS-like visualViewport (layout viewport, i.e. innerHeight, never shrinks) + safe-area insets
  await page.addInitScript(([SAB, SAT]) => {
    Object.defineProperty(navigator, "standalone", { value: true });
    const vv = new EventTarget();
    Object.assign(vv, { width: innerWidth, height: innerHeight, offsetTop: 0, offsetLeft: 0, scale: 1 });
    Object.defineProperty(window, "visualViewport", { value: vv });
    window.__setKb = (kb) => { vv.height = innerHeight - kb; vv.dispatchEvent(new Event("resize")); };
    document.addEventListener("DOMContentLoaded", () => {
      const s = document.createElement("style");
      s.textContent = "html.phone{--sab:" + SAB + "px !important;--sat:" + SAT + "px !important}";
      document.head.appendChild(s);
    });
  }, [SAB, SAT]);
  await page.goto("http://factory.test/#/planner");
  await page.waitForSelector("#p-body .bubble");
  await page.waitForTimeout(600);

  const sleep = (ms) => page.waitForTimeout(ms);
  // gap = composer top - last bubble bottom (must be >= 0). Also the last bubble must not be hidden by the tab bar.
  const measure = () => page.evaluate(() => {
    const b = [...document.querySelectorAll("#p-body .bubble")].pop().getBoundingClientRect();
    const c = document.getElementById("p-composer").getBoundingClientRect();
    return { gap: Math.round((c.top - b.bottom) * 10) / 10, bubbleBottom: b.bottom, composerTop: c.top, scrollY: scrollY,
      max: document.documentElement.scrollHeight - innerHeight, kb: document.documentElement.classList.contains("kb-open") };
  });
  const check = async (name, opts = {}) => {
    const m = await measure();
    const ok = m.gap >= 0 && (!opts.atBottom || m.max - m.scrollY <= 2 || m.bubbleBottom <= m.composerTop);
    if (!ok || m.gap < 0) fails.push(name + ": last message overlaps composer " + JSON.stringify(m));
    return m;
  };
  const toBottom = async () => { await page.evaluate(() => scrollTo(0, document.documentElement.scrollHeight)); await sleep(100); };
  const stateOK = (name, m) => { if (m.gap < 0) fails.push(name + " " + JSON.stringify(m)); };

  // 1. initial load (page scrolls to bottom on first render)
  stateOK("initial load", await measure());
  // 2. after scrolling to the very bottom, and while scrolling up/down
  await toBottom(); stateOK("scrolled to bottom", await measure());
  for (const y of [0, 300, 900]) { await page.evaluate((y) => scrollTo(0, y), y); await sleep(50); }
  await toBottom(); stateOK("scroll round trip", await measure());

  // 3. textarea grows while typing (and shrinks again)
  await page.click("#p-input");
  await page.evaluate(() => __setKb(336)); await sleep(500);
  stateOK("focus + keyboard open", await measure());
  const line = "typing a longer message that wraps ";
  for (let i = 1; i <= 8; i++) {
    await page.fill("#p-input", Array(i).fill(line + i).join("\n"));
    await page.dispatchEvent("#p-input", "input"); await sleep(150);
    stateOK("textarea grows (" + i + " lines)", await measure());
  }
  await page.fill("#p-input", "x"); await page.dispatchEvent("#p-input", "input"); await sleep(200);
  stateOK("textarea shrinks", await measure());
  await page.evaluate(() => __setKb(0)); await page.evaluate(() => document.activeElement.blur()); await sleep(500);
  await toBottom(); stateOK("keyboard closed", await measure());

  // 4. keyboard opens while the user is at the bottom: must stay at the bottom, last message visible
  await page.evaluate(() => __setKb(336)); await sleep(600);
  const k = await measure(); stateOK("keyboard open at bottom", k);
  if (k.max - k.scrollY > 4) fails.push("keyboard open: was at bottom but got left " + (k.max - k.scrollY) + "px above it");

  // 5. user scrolled up to read history: growth must NOT yank them down
  await page.evaluate(() => __setKb(0)); await sleep(300);
  await page.evaluate(() => scrollTo(0, 200)); await sleep(150);
  const y0 = await page.evaluate(() => scrollY);
  await page.fill("#p-input", Array(6).fill(line).join("\n")); await page.dispatchEvent("#p-input", "input"); await sleep(300);
  const y1 = await page.evaluate(() => scrollY);
  if (Math.abs(y1 - y0) > 2) fails.push("scrolled-up user was moved: " + y0 + " -> " + y1);
  await page.fill("#p-input", ""); await page.dispatchEvent("#p-input", "input"); await sleep(200);

  // 6. new message arrives (at bottom: follow, last bubble clear of composer)
  await toBottom();
  msgs = thread(15);
  await page.evaluate(() => pLoad()); await sleep(500);
  stateOK("new message arrives", await measure());

  // 7. orientation change (landscape is not phone-width via max-width, so check portrait->landscape->portrait on a width <=760 box too)
  await page.setViewportSize({ width: 740, height: 360 }); await sleep(500);
  await page.evaluate(() => window.dispatchEvent(new Event("orientationchange")));
  await toBottom(); stateOK("landscape 740x360", await measure());
  await page.setViewportSize({ width: W, height: H }); await sleep(500);
  await toBottom(); stateOK("back to portrait", await measure());

  // 8. real composer height vs --composer-h variable is tracked
  const v = await page.evaluate(() => ({ css: parseFloat(getComputedStyle(document.documentElement).getPropertyValue("--composer-h")), real: document.getElementById("p-composer").offsetHeight }));
  if (Math.abs(v.css - v.real) > 1) fails.push("--composer-h (" + v.css + ") != composer height (" + v.real + ")");

  await browser.close();
  if (fails.length) { console.log(fails.join("\n")); process.exit(1); }
  console.log("ok");
})().catch((e) => { console.log(e.stack || e); process.exit(1); });
