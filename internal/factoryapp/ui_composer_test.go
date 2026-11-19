package factoryapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUIComposerEnterWiring pins the composer key rules: Enter sends only on
// desktop (via enterSends), touch/phone input gets a newline, the desktop
// composers grow to their content, and the phone composer has no Enter-submit.
func TestUIComposerEnterWiring(t *testing.T) {
	ui := string(uiHTML)
	for _, want := range []string{
		`const isTouchInput = () => isPhone() || matchMedia("(pointer: coarse)").matches;`,
		`function enterSends(ev, touch) {`,
		`ev.isComposing || ev.keyCode === 229`,
		`if (enterSends(ev, isTouchInput())) { ev.preventDefault(); $("chatform").requestSubmit(); }`,
		`if (enterSends(ev, isTouchInput())) { ev.preventDefault(); $("newtaskform").requestSubmit(); }`,
		`function fitTextarea(ta, maxPx) {`,
		`$("chat-input").addEventListener("input", () => fitTextarea($("chat-input"), 140));`,
		`$("newtask-input").addEventListener("input", () => fitTextarea($("newtask-input"), 140));`,
		`$("chat-input").value = ""; fitTextarea($("chat-input"), 140);`,
	} {
		if !strings.Contains(ui, want) {
			t.Errorf("composer wiring missing %q", want)
		}
	}
	// the phone composer must never submit on Enter
	i := strings.Index(ui, `const ta = $("p-input");`+"\n  ta.addEventListener(\"input\", pAutosize);")
	if i < 0 {
		t.Fatal("phone composer wiring not found")
	}
	if j := strings.Index(ui[i:], "$(\"p-composer\").onsubmit"); j < 0 ||
		strings.Contains(ui[i:i+j], "keydown") {
		t.Error("phone composer must not bind an Enter-to-send keydown")
	}
	// no bare Enter-sends remain on the desktop composers
	if strings.Contains(ui, `if (ev.key === "Enter" && !ev.shiftKey) { ev.preventDefault(); $("chatform")`) ||
		strings.Contains(ui, `if (ev.key === "Enter" && !ev.shiftKey) { ev.preventDefault(); $("newtaskform")`) {
		t.Error("composers must use enterSends (IME- and touch-aware)")
	}
}

// TestUIEnterSendsJS runs enterSends under node (skipped when node is absent,
// like the other UI JS tests) over a table of key events.
func TestUIEnterSendsJS(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	ui := string(uiHTML)
	a := strings.Index(ui, "function enterSends(ev, touch) {")
	if a < 0 {
		t.Fatal("enterSends not found")
	}
	b := strings.Index(ui[a:], "\n}\n")
	if b < 0 {
		t.Fatal("enterSends end not found")
	}
	fn := ui[a : a+b+3]
	const script = `
const assert = require("assert");
%s
const ev = (o) => Object.assign({ key: "Enter", shiftKey: false, isComposing: false, keyCode: 13 }, o);
const cases = [
  ["desktop enter sends", ev({}), false, true],
  ["desktop shift+enter newline", ev({ shiftKey: true }), false, false],
  ["touch enter newline", ev({}), true, false],
  ["touch shift+enter newline", ev({ shiftKey: true }), true, false],
  ["desktop mid-IME enter no send", ev({ isComposing: true }), false, false],
  ["desktop keyCode 229 (IME) no send", ev({ keyCode: 229 }), false, false],
  ["touch mid-IME no send", ev({ isComposing: true }), true, false],
  ["desktop letter key", ev({ key: "a" }), false, false],
];
for (const [name, e, touch, want] of cases) assert.strictEqual(enterSends(e, touch), want, name);
console.log("ok");
`
	dir := t.TempDir()
	f := filepath.Join(dir, "enter.js")
	if err := os.WriteFile(f, []byte(strings.ReplaceAll(script, "%s", fn)), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("enterSends: %v\n%s", err, out)
	}
}
