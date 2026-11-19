package factoryapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUIPhoneComposerLayoutLoopSafe pins the loop-safe wiring that keeps the
// phone Planner/thread list clear of the fixed composer (PR #97 made the iPhone
// UI flash with a measure -> pad -> scroll -> measure cycle; see #98):
// the visualViewport handler only reads and schedules, all writes happen in one
// coalesced rAF and are change-guarded, the padding never uses the
// scroll-dependent --kb, and no scroll listener drives layout.
func TestUIPhoneComposerLayoutLoopSafe(t *testing.T) {
	ui := string(uiHTML)
	for _, want := range []string{
		`function pLayoutSoon() {`,
		`  if (pLay.queued) return;`,
		`const pMoved = (a, b) => a === null || Math.abs(a - b) > 1;`,
		`if (pad && stick && pChatRoute()) window.scrollTo(0, document.documentElement.scrollHeight);`,
		`new ResizeObserver(pLayoutSoon).observe($("p-composer"))`,
		`visualViewport.addEventListener("scroll", pViewport, { passive: true })`,
		`html.p-planner #p-body { padding-bottom: calc(var(--tabh) + var(--sab) + var(--composer-h) + 16px); }`,
		`html.p-planner.kb-open #p-body { padding-bottom: calc(var(--composer-h) + var(--sab) + var(--kbpad) + 16px); }`,
		`html.p-thread #p-body, html.p-fthread.kb-open #p-body { padding-bottom: calc(var(--composer-h) + var(--sab) + var(--kbpad) + 16px); }`,
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("composer layout wiring missing %q", want)
		}
	}
	// the visualViewport resize/scroll handler must not write, measure layout or scroll
	a := strings.Index(ui, "function pViewport() {")
	if a < 0 {
		t.Fatal("pViewport not found")
	}
	b := strings.Index(ui[a:], "\n}\n")
	body := ui[a : a+b]
	for _, bad := range []string{"setProperty", "scrollTo", "pScrollBottom", "classList.toggle", "offsetHeight", "getBoundingClientRect", "pLayout()"} {
		if strings.Contains(body, bad) {
			t.Errorf("pViewport (runs on every visualViewport scroll) must only read and schedule, found %q", bad)
		}
	}
	// the list padding must not follow the scroll-dependent --kb
	for _, line := range strings.Split(ui, "\n") {
		if strings.Contains(line, "#p-body { padding-bottom") && strings.Contains(line, "var(--kb)") {
			t.Errorf("#p-body padding must use --kbpad, not the scroll-dependent --kb: %s", strings.TrimSpace(line))
		}
	}
	// and nothing re-measures on window scroll
	for _, bad := range []string{`window.addEventListener("scroll", pLayout`, `window.addEventListener("scroll", pViewport`} {
		if strings.Contains(ui, bad) {
			t.Errorf("layout must not run from a scroll handler: %q", bad)
		}
	}
}

// TestUIPhoneComposerLayoutBrowser drives the real page in headless Chromium
// (testdata/composer_layout.js) at 390x844 with an iOS-like visualViewport and
// asserts (1) the last message stays above the composer on load, scroll,
// focus/keyboard, textarea growth and new messages, and (2) there is no layout
// feedback loop: zero layout writes/scrolls over 2s idle, and none while a storm
// of visualViewport resize+scroll events fires. Needs Playwright + Chromium:
// set PLAYWRIGHT_NODE_PATH to a node_modules dir containing `playwright` (and
// CHROMIUM_PATH if its bundled browser is not installed); skipped otherwise.
func TestUIPhoneComposerLayoutBrowser(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	nm := os.Getenv("PLAYWRIGHT_NODE_PATH")
	if nm == "" {
		t.Skip("PLAYWRIGHT_NODE_PATH not set (needs a node_modules with playwright)")
	}
	dir := t.TempDir()
	page := filepath.Join(dir, "ui.html")
	if err := os.WriteFile(page, uiHTML, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(node, filepath.Join("testdata", "composer_layout.js"), page)
	cmd.Env = append(os.Environ(), "NODE_PATH="+nm)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("browser layout check failed: %v\n%s", err, out)
	}
}
