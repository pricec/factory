package factoryapp_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/factoryapp"
)

const secretMarker = "VALUE-MARKER-s3cr3t-9f1c"

func secretsDo(t *testing.T, e *testEnv, method, path, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestSecretsHTTP(t *testing.T) {
	e := newEnv(t)
	tests := []struct {
		name, method, path, body string
		status                   int
		contains                 []string
	}{
		{
			"create default env", "POST", "/v1/secrets/gh-token", `{"value":"` + secretMarker + `"}`, 200,
			[]string{`"name":"gh-token"`, `"env_var":"SECRET_GH_TOKEN"`, `"updated_at"`},
		},
		{
			"upsert custom env", "POST", "/v1/secrets/gh-token", `{"value":"` + secretMarker + `2","env_var":"GH_TOKEN"}`, 200,
			[]string{`"env_var":"GH_TOKEN"`},
		},
		{"second secret", "POST", "/v1/secrets/aws", `{"value":"` + secretMarker + `3"}`, 200, nil},
		{
			"list", "GET", "/v1/secrets", "", 200,
			[]string{`"secrets":[`, `"name":"aws"`, `"name":"gh-token"`, `"env_var":"GH_TOKEN"`},
		},
		{"bad name", "POST", "/v1/secrets/Bad_Name", `{"value":"` + secretMarker + `"}`, 400, []string{`"code":"bad_request"`, `"retryable":false`}},
		{"bad env", "POST", "/v1/secrets/ok", `{"value":"` + secretMarker + `","env_var":"lower"}`, 400, []string{`"error"`}},
		{"empty value", "POST", "/v1/secrets/ok", `{"value":""}`, 400, []string{`"error"`}},
		{"too big", "POST", "/v1/secrets/ok", `{"value":"` + strings.Repeat("x", 64<<10+1) + `"}`, 400, []string{`"error"`}},
		{"malformed json", "POST", "/v1/secrets/ok", `{"value":"` + secretMarker, 400, []string{`"error"`}},
		{"no value route", "GET", "/v1/secrets/gh-token", "", 405, nil},
		{"delete", "DELETE", "/v1/secrets/gh-token", "", 200, []string{`"name":"gh-token"`}},
		{"delete unknown", "DELETE", "/v1/secrets/gh-token", "", 404, []string{`"code":"unknown_secret"`}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, body := secretsDo(t, e, tc.method, tc.path, tc.body)
			if status != tc.status {
				t.Fatalf("status = %d, want %d; body %s", status, tc.status, body)
			}
			for _, w := range tc.contains {
				if !strings.Contains(body, w) {
					t.Errorf("body missing %q: %s", w, body)
				}
			}
			// Raw bytes: no response, success or error, may carry a value.
			if strings.Contains(body, secretMarker) {
				t.Errorf("response leaks the secret value: %s", body)
			}
		})
	}
	if _, body := secretsDo(t, e, "GET", "/v1/secrets", ""); strings.Contains(body, "gh-token") || strings.Contains(body, secretMarker) {
		t.Errorf("list after delete: %s", body)
	}
}

// TestSecretsFileMode: the store file lives in the data dir at 0600.
func TestSecretsFileMode(t *testing.T) {
	dir := t.TempDir()
	a, err := factoryapp.New(factoryapp.Config{DataDir: dir, Addr: "127.0.0.1:0", AdvertiseURL: testAdvertiseURL})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	rec := httptest.NewRecorder()
	a.Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/v1/secrets/k", strings.NewReader(`{"value":"v"}`)))
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	fi, err := os.Stat(filepath.Join(dir, "secrets.json"))
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("secrets.json: %v, %v", fi, err)
	}
}

// TestUICredentialsPanel: string contract for the Credentials section.
func TestUICredentialsPanel(t *testing.T) {
	e := newEnv(t)
	_, page := secretsDo(t, e, "GET", "/", "")
	for _, want := range []string{
		`id="credpanel"`, `id="c-list"`, `id="c-form"`, `id="c-name"`, `id="c-env"`, `id="c-err"`,
		`id="c-value" type="password"`, `getJSON("/v1/secrets")`, `"/v1/secrets/" + encodeURIComponent(name)`,
		`method: "DELETE"`, `$("c-value").value = ""`, "save failed: ", "delete failed: ",
		"c.name", "c.env_var", "c.updated_at", "d.secrets",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("UI page missing %q", want)
		}
	}
	// Write-only: nothing in the page reads a value back from the server.
	for _, bad := range []string{"c.value", "s.value", ".secret_value"} {
		if strings.Contains(page, bad) {
			t.Errorf("UI page renders a secret value (%q)", bad)
		}
	}
}
