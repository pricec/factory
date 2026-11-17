package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCapabilitiesHas(t *testing.T) {
	t.Parallel()
	caps := Capabilities{"os": "linux", "gpu": "", "tools": "git,docker"}
	tests := []struct {
		name string
		tag  string
		want bool
	}{
		{"present non-empty", "os", true},
		{"missing", "arch", false},
		{"empty value counts as absent", "gpu", false},
		{"name:value exact", "os:linux", true},
		{"name:value mismatch", "os:darwin", false},
		{"name:value missing tag", "arch:amd64", false},
		{"name:value over empty-valued tag", "gpu:a100", false},
		{"name:value comma-list member", "tools:git", true},
		{"name:value comma-list second member", "tools:docker", true},
		{"name:value not in comma-list", "tools:rust", false},
		{"empty value part degrades to bare name", "os:", true},
		{"empty value part over empty tag", "gpu:", false},
		{"empty name part", ":linux", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := caps.Has(tt.tag); got != tt.want {
				t.Errorf("Has(%q) = %v, want %v", tt.tag, got, tt.want)
			}
		})
	}
}

func TestCapabilitiesHasAll(t *testing.T) {
	t.Parallel()
	caps := Capabilities{"os": "linux", "arch": "amd64", "gpu": "a100", "tools": "git,docker"}
	tests := []struct {
		name     string
		caps     Capabilities
		requires []string
		want     bool
	}{
		{"empty requires matches anything", caps, nil, true},
		{"empty requires matches no-cap foreman", Capabilities{}, []string{}, true},
		{"nil caps, empty requires", nil, nil, true},
		{"subset satisfied", caps, []string{"os", "gpu"}, true},
		{"missing one tag", caps, []string{"os", "tools", "bigmem"}, false},
		{"nil caps, non-empty requires", nil, []string{"os"}, false},
		{"name:value entries satisfied", caps, []string{"os:linux", "arch:amd64", "tools:git"}, true},
		{"name:value one mismatch", caps, []string{"os:linux", "arch:arm64"}, false},
		{"name:value missing tag", caps, []string{"os:linux", "bigmem:yes"}, false},
		{"bare values never match tag keys", caps, []string{"linux", "amd64"}, false},
		{"nil caps, name:value requires", nil, []string{"os:linux"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.caps.HasAll(tt.requires); got != tt.want {
				t.Errorf("HasAll(%v) = %v, want %v", tt.requires, got, tt.want)
			}
		})
	}
}

func TestCapabilitiesTags(t *testing.T) {
	t.Parallel()
	caps := Capabilities{"tools": "git", "os": "linux", "gpu": ""}
	got := caps.Tags()
	want := []string{"gpu", "os", "tools"}
	if len(got) != len(want) {
		t.Fatalf("Tags() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Tags() = %v, want %v", got, want)
		}
	}
	if got := (Capabilities{}).Tags(); len(got) != 0 {
		t.Fatalf("Tags() on empty caps = %v, want empty", got)
	}
}

type statusErr struct {
	status int
	msg    string
}

func (e *statusErr) Error() string   { return e.msg }
func (e *statusErr) HTTPStatus() int { return e.status }

func TestRefreshHandler(t *testing.T) {
	t.Parallel()
	type upsertCall struct {
		identity string
		caps     map[string]string
	}
	authzOK := func(r *http.Request, identity *string) error {
		*identity = "foreman:alpha"
		return nil
	}
	tests := []struct {
		name       string
		authz      func(r *http.Request, identity *string) error
		upsert     func(ctx context.Context, identity string, caps map[string]string) error
		method     string
		body       string
		wantStatus int
		wantCode   string // empty body -> no error envelope expected
		wantUpsert *upsertCall
	}{
		{
			name:   "happy path: upsert receives identity and caps, 204",
			authz:  authzOK,
			method: http.MethodPost,
			body:   `{"capabilities":{"os":"linux","tools":"git,docker"}}`,
			wantUpsert: &upsertCall{
				identity: "foreman:alpha",
				caps:     map[string]string{"os": "linux", "tools": "git,docker"},
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "authz error without status maps to 401",
			authz:      func(r *http.Request, identity *string) error { return errors.New("nope") },
			method:     http.MethodPost,
			body:       `{"capabilities":{}}`,
			wantStatus: http.StatusUnauthorized,
			wantCode:   "unauthorized",
		},
		{
			name:       "authz error carrying HTTPStatus maps to it",
			authz:      func(r *http.Request, identity *string) error { return &statusErr{status: 403, msg: "wrong scope"} },
			method:     http.MethodPost,
			body:       `{"capabilities":{}}`,
			wantStatus: http.StatusForbidden,
			wantCode:   "forbidden",
		},
		{
			name:       "authz resolves empty identity -> 500",
			authz:      func(r *http.Request, identity *string) error { return nil },
			method:     http.MethodPost,
			body:       `{"capabilities":{}}`,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
		{
			name:       "invalid JSON -> 400",
			authz:      authzOK,
			method:     http.MethodPost,
			body:       `{"capabilities":`,
			wantStatus: http.StatusBadRequest,
			wantCode:   "bad_request",
		},
		{
			name:  "upsert error -> 500 retryable",
			authz: authzOK,
			upsert: func(ctx context.Context, identity string, caps map[string]string) error {
				return errors.New("store down")
			},
			method:     http.MethodPost,
			body:       `{"capabilities":{"os":"linux"}}`,
			wantStatus: http.StatusInternalServerError,
			wantCode:   "internal",
		},
		{
			name:       "non-POST method -> 405",
			authz:      authzOK,
			method:     http.MethodGet,
			wantStatus: http.StatusMethodNotAllowed,
			wantCode:   "method_not_allowed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got upsertCall
			upsert := tt.upsert
			if upsert == nil && tt.wantUpsert != nil {
				upsert = func(ctx context.Context, identity string, caps map[string]string) error {
					got = upsertCall{identity: identity, caps: caps}
					return nil
				}
			} else if upsert == nil {
				upsert = func(ctx context.Context, identity string, caps map[string]string) error {
					t.Errorf("upsert unexpectedly called with (%q, %v)", identity, caps)
					return nil
				}
			}
			h := RefreshHandler(tt.authz, upsert)
			var bodyReader *bytes.Reader
			if tt.body == "" {
				bodyReader = bytes.NewReader(nil)
			} else {
				bodyReader = bytes.NewReader([]byte(tt.body))
			}
			req := httptest.NewRequest(tt.method, "/v1/foreman/capabilities", bodyReader)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body: %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			var errEnvelope struct {
				Error struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				} `json:"error"`
			}
			if tt.wantCode == "" {
				if rec.Body.Len() != 0 {
					t.Fatalf("want empty body, got %q", rec.Body.String())
				}
			} else {
				if err := json.Unmarshal(rec.Body.Bytes(), &errEnvelope); err != nil {
					t.Fatalf("decode error body %q: %v", rec.Body.String(), err)
				}
				if errEnvelope.Error.Code != tt.wantCode {
					t.Fatalf("code = %q, want %q (body: %s)", errEnvelope.Error.Code, tt.wantCode, rec.Body.String())
				}
			}
			if tt.wantUpsert != nil {
				if got.identity != tt.wantUpsert.identity {
					t.Errorf("upsert identity = %q, want %q", got.identity, tt.wantUpsert.identity)
				}
				if len(got.caps) != len(tt.wantUpsert.caps) {
					t.Errorf("upsert caps = %v, want %v", got.caps, tt.wantUpsert.caps)
				} else {
					for k, v := range tt.wantUpsert.caps {
						if got.caps[k] != v {
							t.Errorf("upsert caps[%q] = %q, want %q", k, got.caps[k], v)
						}
					}
				}
			}
		})
	}
}

func TestRefreshHandlerRetryableFlag(t *testing.T) {
	t.Parallel()
	h := RefreshHandler(
		func(r *http.Request, identity *string) error { return errors.New("bad token") },
		nil,
	)
	req := httptest.NewRequest(http.MethodPost, "/v1/foreman/capabilities", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Error.Retryable {
		t.Errorf("retryable = true, want false for auth failures")
	}
}

func TestDetectTools(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// git: executable file -> detected.
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// docker: non-executable file -> not detected.
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	// opencode: a directory named like the tool -> not detected.
	if err := os.Mkdir(filepath.Join(dir, "opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	caps := DetectTools([]string{dir})
	tools, ok := caps["tools"]
	if !ok {
		t.Fatalf("DetectTools missing %q key: %v", "tools", caps)
	}
	if tools == "" {
		t.Fatalf("tools = %q, want it to contain git", tools)
	}
	for _, name := range strings.Split(tools, ",") {
		switch name {
		case "git":
			// expected
		case "docker", "opencode":
			t.Errorf("tools = %q, %q must be absent (not executable / a directory)", tools, name)
		default:
			t.Errorf("tools = %q, unexpected entry %q", tools, name)
		}
	}
}

func TestDetectToolsSortedAndAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"opencode", "docker", "git"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	caps := DetectTools([]string{dir})
	if got := caps["tools"]; got != "docker,git,opencode" {
		t.Errorf("tools = %q, want %q (sorted, comma-joined)", got, "docker,git,opencode")
	}
	// Nothing on PATH: empty tools value.
	caps = DetectTools([]string{t.TempDir()})
	if got := caps["tools"]; got != "" {
		t.Errorf("tools = %q, want empty when nothing is detected", got)
	}
}

func TestRefreshReconcilesOnlyWhenActiveJobsReported(t *testing.T) {
	authz := func(r *http.Request, identity *string) error { *identity = "foreman:alpha"; return nil }
	upsert := func(context.Context, string, map[string]string) error { return nil }
	for _, tc := range []struct {
		body string
		want []string
		call bool
	}{
		{`{"capabilities":{}}`, nil, false},                                      // older foreman: no claim
		{`{"capabilities":{},"active_jobs":[]}`, []string{}, true},               // restarted: claims nothing
		{`{"capabilities":{},"active_jobs":["job_a"]}`, []string{"job_a"}, true}, // claims one
	} {
		var got []string
		called := false
		h := RefreshHandlerReconciling(authz, upsert, nil, func(_ context.Context, id string, active []string) error {
			called, got = true, active
			if id != "foreman:alpha" {
				t.Errorf("identity = %q", id)
			}
			return nil
		})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/foreman/capabilities", strings.NewReader(tc.body)))
		if rec.Code != http.StatusNoContent || called != tc.call || len(got) != len(tc.want) {
			t.Errorf("body %s: code=%d called=%v got=%v", tc.body, rec.Code, called, got)
		}
	}
}
