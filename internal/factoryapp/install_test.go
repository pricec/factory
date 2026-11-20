package factoryapp_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/factoryapp"
)

// TestInstallScriptHappyPath: the planner bearer fetches the rendered
// install script - text/x-shellscript, carrying the advertise URL and a
// 64-hex pairing token that actually pairs a foreman end to end (#24).
func TestInstallScriptHappyPath(t *testing.T) {
	e := newEnv(t)

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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /install.sh status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/x-shellscript" {
		t.Fatalf("Content-Type = %q, want text/x-shellscript", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)

	// The rendered script targets the factory's advertised URL and no
	// server-side placeholders survive rendering.
	if !strings.Contains(script, testAdvertiseURL) {
		t.Fatalf("script does not contain advertise URL %q", testAdvertiseURL)
	}
	for _, ph := range []string{"{{FACTORY_URL}}", "{{PAIRING_TOKEN}}"} {
		if strings.Contains(script, ph) {
			t.Fatalf("script still contains placeholder %q", ph)
		}
	}

	// The embedded token is a 64-hex one-time pairing token and pairs a
	// foreman via POST /v1/pair (spec §7.1).
	re := regexp.MustCompile(`(?m)^PAIRING_TOKEN="([0-9a-f]{64})"$`)
	m := re.FindStringSubmatch(script)
	if m == nil {
		t.Fatalf("no 64-hex PAIRING_TOKEN in rendered script:\n%s", script)
	}
	var pairResp struct {
		Bearer    string `json:"bearer"`
		InboxID   string `json:"inbox_id"`
		ParentURL string `json:"parent_url"`
	}
	if status := e.do("POST", "/v1/pair", "", map[string]any{
		"pairing_token": m[1],
		"self":          map[string]any{"id": "onboarded", "capabilities": map[string]string{}},
	}, &pairResp); status != http.StatusOK {
		t.Fatalf("pair with embedded token: status %d", status)
	}
	if pairResp.InboxID != "foreman:onboarded" || pairResp.ParentURL != testAdvertiseURL {
		t.Fatalf("pair response = %+v", pairResp)
	}
}

// TestInstallScriptBashSyntax: the rendered script must pass bash -n.
func TestInstallScriptBashSyntax(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skipf("bash not available: %v", err)
	}
	e := newEnv(t)
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
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bash, "-n")
	cmd.Stdin = bytes.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
}

// TestInstallScriptAuth: /install.sh mirrors POST /v1/pairing-tokens -
// 401 unknown bearer, 403 non-planner identity (#24, spec §7.1).
func TestInstallScriptAuth(t *testing.T) {
	e := newEnv(t)
	foremanBearer := e.pair("alpha", nil)

	for _, tc := range []struct {
		name   string
		bearer string
		status int
		code   string
	}{
		{"foreman bearer", foremanBearer, http.StatusForbidden, "forbidden"},
		{"unknown bearer", "bearer-that-does-not-exist", http.StatusUnauthorized, "unauthorized"},
		{"missing authorization", "", http.StatusUnauthorized, "unauthorized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest("GET", e.ts.URL+"/install.sh", nil)
			if err != nil {
				t.Fatal(err)
			}
			if tc.bearer != "" {
				req.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			var errBody struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&errBody); err != nil {
				t.Fatal(err)
			}
			if errBody.Error.Code != tc.code {
				t.Fatalf("error code = %q, want %q", errBody.Error.Code, tc.code)
			}
		})
	}
}

// TestDownload: GET /download/foreman/{platform} serves the exact bytes
// from DownloadsDir, 404 for a missing binary, and never 200 on a
// traversal-ish platform (the anchored regexp is the path-safety gate).
func TestDownload(t *testing.T) {
	dir := t.TempDir()
	seed := []byte("\x7fELF-fake-foreman-binary")
	if err := os.WriteFile(filepath.Join(dir, "foreman-linux-amd64"), seed, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *factoryapp.Config) { c.DownloadsDir = dir })

	// Unauthenticated (trusted-LAN posture, spec §7.5): exact bytes back.
	req, err := http.NewRequest("GET", e.ts.URL+"/download/foreman/foreman-linux-amd64", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("Content-Type = %q, want application/octet-stream", ct)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, seed) {
		t.Fatalf("downloaded bytes = %q, want %q", got, seed)
	}

	// Missing binary (arm64 not built): 404. Bad platform: 400. Traversal:
	// never 200 - the mux cleans the path (404) and the regexp rejects
	// what reaches the handler (400); both are acceptable "blocked".
	for _, tc := range []struct {
		platform string
		status   int
	}{
		{"foreman-linux-arm64", http.StatusNotFound},
		{"foreman-linux-riscv64", http.StatusBadRequest},
		{"../etc/passwd", http.StatusNotFound},
		{"..%2Fetc%2Fpasswd", http.StatusNotFound},
	} {
		t.Run(tc.platform, func(t *testing.T) {
			req, err := http.NewRequest("GET", e.ts.URL+"/download/foreman/"+strings.ReplaceAll(tc.platform, "%2F", "/"), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
		})
	}
}

// TestInstallServer: the planner bearer fetches the rendered factory-server
// bootstrap script (#25) - text/x-shellscript, carrying the advertise URL,
// syntactically valid bash, and the pinned structure (pre-seed URL, token
// generation/reuse, root-only unit, ExecStart flags).
func TestInstallServer(t *testing.T) {
	e := newEnv(t)

	get := func(bearer string) (int, string) {
		t.Helper()
		req, err := http.NewRequest("GET", e.ts.URL+"/install-server.sh", nil)
		if err != nil {
			t.Fatal(err)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, string(b)
	}

	foremanBearer := e.pair("alpha", nil)
	for _, tc := range []struct {
		name   string
		bearer string
		status int
		code   string
	}{
		{"foreman bearer", foremanBearer, http.StatusForbidden, "forbidden"},
		{"unknown bearer", "bearer-that-does-not-exist", http.StatusUnauthorized, "unauthorized"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := get(tc.bearer)
			if status != tc.status {
				t.Fatalf("status = %d, want %d", status, tc.status)
			}
			var errBody struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(body), &errBody); err != nil {
				t.Fatal(err)
			}
			if errBody.Error.Code != tc.code {
				t.Fatalf("error code = %q, want %q", errBody.Error.Code, tc.code)
			}
		})
	}

	status, script := get(e.a.PlannerToken())
	if status != http.StatusOK {
		t.Fatalf("GET /install-server.sh status = %d, want 200", status)
	}
	if !strings.Contains(script, testAdvertiseURL) {
		t.Fatalf("script does not contain advertise URL %q", testAdvertiseURL)
	}
	if strings.Contains(script, "{{FACTORY_URL}}") {
		t.Fatal("script still contains the {{FACTORY_URL}} placeholder")
	}

	// Structural pins (spec §7.1, #25): what we ship, asserted verbatim.
	// The download URLs must use the servable platform names the server's
	// anchored regexp accepts: {kind}-linux-{arch} (a bare linux-$GOARCH
	// path 400s - issue "install.sh downloads the foreman binary from the
	// wrong URL").
	for _, want := range []string{
		"/download/factory/factory-linux-$GOARCH",
		"/download/foreman/foreman-linux-",
		"FACTORY_PLANNER_TOKEN",
		`sed -n 's/^Environment=FACTORY_PLANNER_TOKEN=\([a-f0-9]\{64\}\)$/\1/p'`,
		"chmod 600 /etc/systemd/system/factory.service",
		"-data /var/lib/factory",
		"EnvironmentFile=/etc/factory/factory.env",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("rendered script missing %q", want)
		}
	}

	// The broken shape (bare linux-$GOARCH platform) must never come back -
	// the server 400s it (unknown platform), which bricked all onboarding.
	for _, bad := range []string{
		"/download/factory/linux-$GOARCH",
		"/download/foreman/linux-$GOARCH",
	} {
		if strings.Contains(script, bad) {
			t.Fatalf("rendered script still uses unservable download URL %q", bad)
		}
	}

	// bash -n the rendered script.
	if bash, err := exec.LookPath("bash"); err == nil {
		cmd := exec.Command(bash, "-n")
		cmd.Stdin = strings.NewReader(script)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("bash -n: %v\n%s", err, out)
		}
	} else {
		t.Skipf("bash not available: %v", err)
	}
}

// TestDownloadFactoryBinary: GET /download/factory/{platform} serves the
// exact bytes from DownloadsDir; unsupported platform and traversal never
// yield 200 (the anchored regexp is the path-safety gate).
func TestDownloadFactoryBinary(t *testing.T) {
	dir := t.TempDir()
	seed := []byte("\x7fELF-fake-factory-binary")
	if err := os.WriteFile(filepath.Join(dir, "factory-linux-amd64"), seed, 0o755); err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, func(c *factoryapp.Config) { c.DownloadsDir = dir })

	req, err := http.NewRequest("GET", e.ts.URL+"/download/factory/factory-linux-amd64", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, seed) {
		t.Fatalf("downloaded bytes = %q, want %q", got, seed)
	}

	// Unsupported arch (regexp miss -> 400) and traversal-ish paths
	// (mux-cleaned 404 / regexp 400): never 200.
	for _, platform := range []string{
		"factory-linux-riscv64",
		"../etc/passwd",
		"..%2Fetc%2Fpasswd",
	} {
		t.Run(platform, func(t *testing.T) {
			req, err := http.NewRequest("GET", e.ts.URL+"/download/factory/"+strings.ReplaceAll(platform, "%2F", "/"), nil)
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Fatalf("status = 200 for %q", platform)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
		})
	}
}

// TestInstallScriptsDownloadServablePlatforms: every download URL rendered
// into /install.sh and /install-server.sh must name a platform the server
// actually serves - the same anchored set handleDownload enforces
// (`^(factory|foreman|vision)-(linux)-(amd64|arm64)$`, spec §7.1). A URL like
// /download/foreman/linux-$GOARCH is rejected with 400 "unknown platform",
// which made every fresh onboarding fail at the curl step.
func TestInstallScriptsDownloadServablePlatforms(t *testing.T) {
	e := newEnv(t)

	fetch := func(path string) string {
		t.Helper()
		req, err := http.NewRequest("GET", e.ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+e.a.PlannerToken())
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	// Same platform set handleDownload serves (install.go downloadPattern).
	servable := regexp.MustCompile(`^(factory|foreman|vision)-(linux)-(amd64|arm64)$`)
	// /download/{kind}/{...} URLs as the scripts curl them ($GOARCH still
	// unexpanded - the shell substitutes amd64|arm64 at runtime).
	urlRe := regexp.MustCompile(`/download/[a-z]+/[A-Za-z0-9.$_-]+`)

	for _, tc := range []struct {
		name   string
		script string
	}{
		{"install.sh", fetch("/install.sh")},
		{"install-server.sh", fetch("/install-server.sh")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := 0
			for _, u := range urlRe.FindAllString(tc.script, -1) {
				platform := u[strings.LastIndex(u, "/")+1:]
				if !servable.MatchString(strings.ReplaceAll(platform, "$GOARCH", "amd64")) {
					t.Fatalf("script curls unservable download URL %q (server serves only {kind}-linux-{amd64,arm64})", u)
				}
				seen++
			}
			if seen == 0 {
				t.Fatal("no download URLs found in script")
			}
		})
	}
}

// TestRenderInstallDeterminism: apart from the per-request token, the
// rendered script is stable across fetches (same inputs -> same template).
func TestRenderInstallDeterminism(t *testing.T) {
	e := newEnv(t)
	get := func() string {
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
	re := regexp.MustCompile(`[0-9a-f]{64}`)
	if a, b := re.ReplaceAllString(get(), "<TOKEN>"), re.ReplaceAllString(get(), "<TOKEN>"); a != b {
		t.Fatalf("rendered template differs between identical renders")
	}
}
