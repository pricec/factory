package factoryapp_test

import (
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func fetchInstall(t *testing.T, e *testEnv) string {
	t.Helper()
	req, err := http.NewRequest("GET", e.ts.URL+"/install.sh", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.a.PlannerToken())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestInstallForemanLayoutAndUnit: the foreman installer downloads into the
// service-writable bin dir, points ExecStart there, migrates the old
// /opt/factory/foreman layout without touching state, and writes the
// crash-loop-guard unit settings (auto-update C5/C8).
func TestInstallForemanLayoutAndUnit(t *testing.T) {
	script := fetchInstall(t, newEnv(t))
	for _, want := range []string{
		"BIN_DIR=/var/lib/factory-foreman/bin",
		`-o "$BIN_DIR/foreman.new"`,
		`mv -f "$BIN_DIR/foreman.new" "$BIN_DIR/foreman"`,
		`chown factory-foreman "$BIN_DIR"`,
		"\nStartLimitIntervalSec=600\nStartLimitBurst=10\n",
		"\nRestart=always\nRestartSec=5\n",
		"\nExecStartPre=+/opt/factory/foreman-update-guard.sh /var/lib/factory-foreman\n",
		"\nExecStart=/var/lib/factory-foreman/bin/foreman -factory " + testAdvertiseURL + " -id {{FOREMAN_ID}} -data /var/lib/factory-foreman -addr 127.0.0.1:8090\n",
		"\nEnvironment=PATH={{SERVICE_PATH}}\n",
		"\nEnvironment=FACTORY_PAIRING_TOKEN=",
		"rm -f /opt/factory/foreman\n", // migration cleanup of the old binary
		"chown root:root /opt/factory /opt/factory/foreman-update-guard.sh",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install.sh lacks %q", want)
		}
	}
	if strings.Contains(script, "ExecStart=/opt/factory/foreman") {
		t.Error("ExecStart still points at the old /opt/factory/foreman")
	}
	// Migration must never delete or rewrite state.
	for _, bad := range []string{"rm -rf", "-o /opt/factory/foreman"} {
		if strings.Contains(script, bad) {
			t.Errorf("install.sh must not contain %q", bad)
		}
	}
	// The guard is installed before the unit is written/started.
	if strings.Index(script, "foreman-update-guard.sh <<'GUARD_EOF'") > strings.Index(script, "cat >/etc/systemd/system/factory-foreman.service") {
		t.Error("guard script must be written before the unit")
	}
}

func extractGuard(t *testing.T, script string) string {
	t.Helper()
	const start = "<<'GUARD_EOF'\n"
	i := strings.Index(script, start)
	if i < 0 {
		t.Fatal("no guard heredoc")
	}
	rest := script[i+len(start):]
	j := strings.Index(rest, "\nGUARD_EOF\n")
	if j < 0 {
		t.Fatal("unterminated guard heredoc")
	}
	return rest[:j+1]
}

type guardEnv struct {
	t     *testing.T
	sh    string
	guard string
	data  string
	bin   string
}

func newGuardEnv(t *testing.T) *guardEnv {
	t.Helper()
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	dir := t.TempDir()
	g := filepath.Join(dir, "guard.sh")
	if err := os.WriteFile(g, []byte(extractGuard(t, fetchInstall(t, newEnv(t)))), 0o755); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	bin := filepath.Join(data, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	return &guardEnv{t: t, sh: sh, guard: g, data: data, bin: bin}
}

func (g *guardEnv) run(args ...string) string {
	g.t.Helper()
	out, err := exec.Command(g.sh, append([]string{g.guard}, args...)...).CombinedOutput()
	if err != nil {
		g.t.Fatalf("guard must always exit 0: %v\n%s", err, out)
	}
	return string(out)
}

func (g *guardEnv) write(name, content string, mode os.FileMode) {
	g.t.Helper()
	if err := os.WriteFile(filepath.Join(g.bin, name), []byte(content), mode); err != nil {
		g.t.Fatal(err)
	}
}

func (g *guardEnv) read(name string) string {
	b, err := os.ReadFile(filepath.Join(g.bin, name))
	if err != nil {
		return "<absent>"
	}
	return string(b)
}

const testMarker = `{"from":"a","to":"b","started_at":"2026-11-16T14:00:00Z","attempts":1}`

func TestGuardInertWithoutMarker(t *testing.T) {
	g := newGuardEnv(t)
	g.write("update-boots", "2", 0o644)
	g.write("foreman", "NEW", 0o755)
	g.write("foreman.prev", "OLD", 0o755)
	g.run(g.data)
	if g.read("update-boots") != "<absent>" {
		t.Error("update-boots must be deleted when no marker")
	}
	if g.read("foreman") != "NEW" {
		t.Error("foreman must be untouched")
	}
}

func TestGuardNoArgsAndMissingDirs(t *testing.T) {
	g := newGuardEnv(t)
	g.run()
	g.run(filepath.Join(g.data, "nonexistent"))
}

func TestGuardCountsThenRollsBack(t *testing.T) {
	g := newGuardEnv(t)
	g.write("update-pending.json", testMarker, 0o644)
	g.write("foreman", "NEW", 0o755)
	g.write("foreman.prev", "OLD", 0o755)

	for i := 1; i <= 2; i++ {
		g.run(g.data)
		if got := strings.TrimSpace(g.read("update-boots")); got != strconv.Itoa(i) {
			t.Fatalf("after start %d update-boots = %q", i, got)
		}
		if g.read("foreman") != "NEW" {
			t.Fatalf("rolled back too early at start %d", i)
		}
	}
	out := g.run(g.data)
	if g.read("foreman") != "OLD" {
		t.Fatalf("third unconfirmed start must restore prev; foreman=%q", g.read("foreman"))
	}
	fi, err := os.Stat(filepath.Join(g.bin, "foreman"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("foreman mode = %v err=%v, want 0755", fi, err)
	}
	if g.read("update-boots") != "<absent>" {
		t.Error("update-boots must be reset after rollback")
	}
	m := g.read("update-pending.json")
	if !strings.Contains(m, `"rolled_back":true`) || !strings.Contains(m, `"to":"b"`) || !strings.HasSuffix(strings.TrimSpace(m), "}") {
		t.Errorf("marker = %q", m)
	}
	if !strings.Contains(out, "rolled back") {
		t.Errorf("guard must log the rollback: %q", out)
	}
	if g.read("foreman.guard.tmp") != "<absent>" {
		t.Error("temp file left behind")
	}

	// Already rolled back: a further burst of unconfirmed starts is a no-op.
	g.write("foreman", "OLD2", 0o755)
	for i := 0; i < 3; i++ {
		g.run(g.data)
	}
	if g.read("foreman") != "OLD2" {
		t.Error("must not roll back twice")
	}
}

func TestGuardRollbackWithoutPrev(t *testing.T) {
	g := newGuardEnv(t)
	g.write("update-pending.json", testMarker, 0o644)
	g.write("foreman", "NEW", 0o755)
	g.write("update-boots", "2", 0o644)
	out := g.run(g.data)
	if g.read("foreman") != "NEW" || !strings.Contains(out, "missing") {
		t.Errorf("no prev: foreman=%q out=%q", g.read("foreman"), out)
	}
	if strings.Contains(g.read("update-pending.json"), "rolled_back") {
		t.Error("marker must not claim a rollback that did not happen")
	}
}

func TestGuardMarkerShapes(t *testing.T) {
	cases := []struct{ name, marker string }{
		{"pretty", "{\n  \"from\": \"a\",\n  \"attempts\": 1\n}\n"},
		{"empty", "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := newGuardEnv(t)
			g.write("update-pending.json", tc.marker, 0o644)
			g.write("foreman", "NEW", 0o755)
			g.write("foreman.prev", "OLD", 0o755)
			g.write("update-boots", "2", 0o644)
			g.run(g.data)
			m := g.read("update-pending.json")
			if !strings.Contains(m, `"rolled_back":true`) {
				t.Fatalf("marker = %q", m)
			}
			if strings.Contains(m, "{,") {
				t.Fatalf("invalid json %q", m)
			}
			if tc.name == "empty" && strings.TrimSpace(m) != `{"rolled_back":true}` {
				t.Fatalf("marker = %q", m)
			}
		})
	}
}

func TestGuardGarbageBootCounter(t *testing.T) {
	g := newGuardEnv(t)
	g.write("update-pending.json", testMarker, 0o644)
	g.write("foreman", "NEW", 0o755)
	g.write("foreman.prev", "OLD", 0o755)
	g.write("update-boots", "abc\n", 0o644)
	g.run(g.data)
	if got := strings.TrimSpace(g.read("update-boots")); got != "1" {
		t.Fatalf("garbage counter must restart at 1, got %q", got)
	}
}
