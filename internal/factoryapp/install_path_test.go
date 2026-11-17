package factoryapp_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestInstallScriptsPinUnitPath: both rendered install scripts pin PATH in
// the systemd unit they write (spec §7.1, §8.4). systemd's default service
// PATH omits the service user's ~/.opencode/bin and ~/.local/bin, so a
// foreman or factory whose unit lacks the pin cannot exec opencode or
// claude (2026-11-16: foremen reported tools caps without opencode). The
// pinned value is the one the install-time verification uses too.
func TestInstallScriptsPinUnitPath(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		path string
		home string
		unit string
	}{
		{"/install.sh", "/var/lib/factory-foreman", "factory-foreman.service"},
		{"/install-server.sh", "/var/lib/factory", "factory.service"},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			req, err := http.NewRequest("GET", e.ts.URL+tc.path, nil)
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
				t.Fatalf("GET %s status = %d, want 200", tc.path, resp.StatusCode)
			}
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			script := string(body)

			wantPath := "SERVICE_PATH=" + tc.home + "/.opencode/bin:" + tc.home + "/.local/bin:" +
				"/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
			if !strings.Contains(script, "\n"+wantPath+"\n") {
				t.Fatalf("script lacks pinned %q", wantPath)
			}
			// The unit pins that same value, substituted at write time.
			if !strings.Contains(script, "\nEnvironment=PATH={{SERVICE_PATH}}\n") {
				t.Fatalf("unit template lacks Environment=PATH placeholder")
			}
			wantSub := `sed -i "s|{{SERVICE_PATH}}|$SERVICE_PATH|" /etc/systemd/system/` + tc.unit
			if !strings.Contains(script, wantSub) {
				t.Fatalf("script does not render SERVICE_PATH into %s:\nwant line %q", tc.unit, wantSub)
			}
		})
	}
}

// TestInstallScriptWritesClaudeModelDropIn: /install.sh writes the Claude
// model env into an installer-owned low-precedence drop-in (so re-runs keep
// operator overrides and never touch pairing) and prints the login step.
func TestInstallScriptWritesClaudeModelDropIn(t *testing.T) {
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
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	script := string(b)
	for _, want := range []string{
		"05-claude-models.conf",
		"\nEnvironment=FACTORY_WORKER_FLASH_MODEL=claude-haiku-5-5\n",
		"\nEnvironment=FACTORY_WORKER_MODEL=claude-sonnet-5-5\n",
		"\nEnvironment=FACTORY_WORKER_HARDEST_MODEL=claude-opus-5-5\n",
		"\nEnvironment=FACTORY_FOREMAN_BRAIN_MODEL=claude-opus-5-5\n",
		"sudo -u $SERVICE_USER -H claude /login",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script lacks %q", want)
		}
	}
}
