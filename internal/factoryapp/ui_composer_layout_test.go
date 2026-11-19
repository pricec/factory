package factoryapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUIPhoneComposerClearsLastMessage pins the wiring that keeps the phone
// Planner/thread message list clear of the fixed composer: the list's bottom
// padding follows the composer's measured top edge (--cmp-inset), re-measured
// on composer resize, visualViewport/window resize, rotation and class flips.
func TestUIPhoneComposerClearsLastMessage(t *testing.T) {
	ui := string(uiHTML)
	for _, want := range []string{
		`function pLayoutComposer() {`,
		`"--cmp-inset"`,
		`new ResizeObserver(pLayoutSoon).observe($("p-composer"))`,
		`addEventListener("orientationchange"`,
		`html.p-planner #p-body { padding-bottom: calc(var(--cmp-inset,`,
		`html.p-planner.kb-open #p-body { padding-bottom: calc(var(--cmp-inset,`,
		`html.p-thread #p-body, html.p-fthread.kb-open #p-body { padding-bottom: calc(var(--cmp-inset,`,
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("composer layout wiring missing %q", want)
		}
	}
}

// TestUIPhoneComposerLayoutBrowser drives the real page in headless Chromium at
// 390x844 (standalone + safe-area stubbed, fake visualViewport for the iOS
// keyboard) and asserts the last message sits above the composer on load,
// scroll, focus/keyboard, textarea growth, new messages and rotation. It needs
// Playwright and a Chromium: set PLAYWRIGHT_NODE_PATH to a node_modules dir
// containing `playwright` (and CHROMIUM_PATH if its bundled browser is not
// installed); otherwise it is skipped, like the other node-based UI tests.
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
