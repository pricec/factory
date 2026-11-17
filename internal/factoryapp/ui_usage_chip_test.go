package factoryapp

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUIUsageChipWiring: the phone chip is driven by five_hour.resets_at, ticks
// client-side, keeps the "5h" fallback and an aria-label.
func TestUIUsageChipWiring(t *testing.T) {
	uiHTML := string(uiHTML)
	for _, want := range []string{
		"usageElapsedFrac", "fmtLeftCompact", "fmtLeftSpoken", "usageRingSVG", "pUsageTick",
		"w.resets_at", `"resets in "`, `<span>5h</span>`, "rotate(-90 7.5 7.5)", "tabular-nums", "-apple-system",
	} {
		if !strings.Contains(uiHTML, want) {
			t.Errorf("UI missing %q", want)
		}
	}
}

// TestUIUsageChipJS runs the pure helpers under node (skipped when node is
// absent; the repo has no other JS runner).
func TestUIUsageChipJS(t *testing.T) {
	uiHTML := string(uiHTML)
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	a := strings.Index(uiHTML, "const USAGE_5H_MS")
	b := strings.Index(uiHTML, "// usageRingSVG")
	if a < 0 || b < a {
		t.Fatal("helper block not found")
	}
	const tests = `
const assert = require("assert");
const H = 3600e3, M = 60e3;
assert.strictEqual(fmtLeftCompact(3*H+42*M+59e3), "3:42");
assert.strictEqual(fmtLeftCompact(H), "1:00");
assert.strictEqual(fmtLeftCompact(H-1000), "59:59");
assert.strictEqual(fmtLeftCompact(42*M), "42:00");
assert.strictEqual(fmtLeftCompact(5*1000), "0:05");
assert.strictEqual(fmtLeftCompact(0), "0:00");
assert.strictEqual(fmtLeftCompact(-5), "0:00");
assert.strictEqual(fmtLeftSpoken(3*H+42*M), "3 hours 42 minutes");
assert.strictEqual(fmtLeftSpoken(H), "1 hour");
assert.strictEqual(fmtLeftSpoken(M), "1 minute");
assert.strictEqual(fmtLeftSpoken(10e3), "less than a minute");
assert.strictEqual(fmtLeftSpoken(-1), "now");
const now = 1e12;
assert.strictEqual(usageElapsedFrac(now + 5*H, now), 0);
assert.strictEqual(usageElapsedFrac(now + 2.5*H, now), 0.5);
assert.strictEqual(usageElapsedFrac(now, now), 1);
assert.strictEqual(usageElapsedFrac(now - H, now), 1);      // past reset
assert.strictEqual(usageElapsedFrac(now + 9*H, now), 0);    // clamped
assert.strictEqual(usageElapsedFrac(null, now), null);      // missing
assert.strictEqual(usageElapsedFrac(NaN, now), null);
assert.strictEqual(usageResetMs(null), null);
assert.strictEqual(usageResetMs("garbage"), null);
assert.strictEqual(usageResetMs("2026-01-01T00:00:00Z"), 1767225600000);
`
	dir := t.TempDir()
	f := filepath.Join(dir, "t.js")
	src := uiHTML[a:b] + tests
	if err := os.WriteFile(f, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
