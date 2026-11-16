package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func writeBin(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeManifest(t *testing.T, dir, raw string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func manifestJSON(build string, files map[string]string) string {
	var parts []string
	for n, s := range files {
		parts = append(parts, fmt.Sprintf(`%q:{"sha256":%q}`, n, s))
	}
	return fmt.Sprintf(`{"build":%q,"files":{%s}}`, build, strings.Join(parts, ","))
}

func newAdv(dir string) (*Advertiser, *[]string) {
	var logs []string
	a := NewAdvertiser(dir)
	a.logf = func(f string, args ...any) { logs = append(logs, fmt.Sprintf(f, args...)) }
	return a, &logs
}

func TestAdvert(t *testing.T) {
	const build = "20261116T141100Z-38a6368abcde"
	tests := []struct {
		name      string
		setup     func(t *testing.T, dir string)
		wantPlats []string // nil -> nil advert
		wantLog   string   // substring; "" -> no log
	}{
		{"valid both platforms", func(t *testing.T, d string) {
			writeBin(t, d, "foreman-linux-amd64", "a")
			writeBin(t, d, "foreman-linux-arm64", "b")
			writeManifest(t, d, manifestJSON(build, map[string]string{
				"foreman-linux-amd64": sum("a"), "foreman-linux-arm64": sum("b"),
				"factory-linux-amd64": sum("ignored-not-on-disk"),
			}))
		}, []string{"linux-amd64", "linux-arm64"}, ""},
		{"missing manifest", func(t *testing.T, d string) {
			writeBin(t, d, "foreman-linux-amd64", "a")
		}, nil, ""},
		{"malformed manifest", func(t *testing.T, d string) {
			writeBin(t, d, "foreman-linux-amd64", "a")
			writeManifest(t, d, "{nope")
		}, nil, "malformed"},
		{"empty build", func(t *testing.T, d string) {
			writeBin(t, d, "foreman-linux-amd64", "a")
			writeManifest(t, d, manifestJSON("", map[string]string{"foreman-linux-amd64": sum("a")}))
		}, nil, "empty build"},
		{"sha mismatch everywhere", func(t *testing.T, d string) {
			writeBin(t, d, "foreman-linux-amd64", "a")
			writeManifest(t, d, manifestJSON(build, map[string]string{"foreman-linux-amd64": sum("other")}))
		}, nil, "mismatch"},
		{"partial: one mismatch", func(t *testing.T, d string) {
			writeBin(t, d, "foreman-linux-amd64", "a")
			writeBin(t, d, "foreman-linux-arm64", "b")
			writeManifest(t, d, manifestJSON(build, map[string]string{
				"foreman-linux-amd64": sum("a"), "foreman-linux-arm64": sum("stale"),
			}))
		}, []string{"linux-amd64"}, "foreman-linux-arm64"},
		{"partial: file missing", func(t *testing.T, d string) {
			writeBin(t, d, "foreman-linux-arm64", "b")
			writeManifest(t, d, manifestJSON(build, map[string]string{
				"foreman-linux-amd64": sum("a"), "foreman-linux-arm64": sum("b"),
			}))
		}, []string{"linux-arm64"}, "foreman-linux-amd64"},
		{"invalid hex sha", func(t *testing.T, d string) {
			writeBin(t, d, "foreman-linux-amd64", "a")
			writeManifest(t, d, manifestJSON(build, map[string]string{"foreman-linux-amd64": "zz"}))
		}, nil, "invalid sha256"},
		{"uppercase manifest sha accepted", func(t *testing.T, d string) {
			writeBin(t, d, "foreman-linux-amd64", "a")
			writeManifest(t, d, manifestJSON(build, map[string]string{"foreman-linux-amd64": strings.ToUpper(sum("a"))}))
		}, []string{"linux-amd64"}, ""},
		{"only factory files", func(t *testing.T, d string) {
			writeBin(t, d, "factory-linux-amd64", "a")
			writeManifest(t, d, manifestJSON(build, map[string]string{"factory-linux-amd64": sum("a")}))
		}, nil, ""},
		{"unsupported foreman platform ignored", func(t *testing.T, d string) {
			writeBin(t, d, "foreman-darwin-arm64", "a")
			writeManifest(t, d, manifestJSON(build, map[string]string{"foreman-darwin-arm64": sum("a")}))
		}, nil, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)
			a, logs := newAdv(dir)
			got := a.Advert()
			if tt.wantPlats == nil {
				if got != nil {
					t.Fatalf("want nil advert, got %+v", got)
				}
			} else {
				if got == nil || got.Build != build || len(got.Platforms) != len(tt.wantPlats) {
					t.Fatalf("advert = %+v, want platforms %v", got, tt.wantPlats)
				}
				for _, p := range tt.wantPlats {
					pa, ok := got.Platforms[p]
					if !ok || pa.Path != "/download/foreman/foreman-"+p || len(pa.SHA256) != 64 {
						t.Errorf("platform %s = %+v", p, pa)
					}
				}
			}
			joined := strings.Join(*logs, "\n")
			if tt.wantLog == "" && joined != "" {
				t.Errorf("unexpected log: %s", joined)
			}
			if tt.wantLog != "" && !strings.Contains(joined, tt.wantLog) {
				t.Errorf("log %q missing %q", joined, tt.wantLog)
			}
		})
	}
}

func TestAdvertLogsOncePerChange(t *testing.T) {
	dir := t.TempDir()
	writeBin(t, dir, "foreman-linux-amd64", "a")
	writeManifest(t, dir, manifestJSON("b1", map[string]string{"foreman-linux-amd64": sum("x")}))
	a, logs := newAdv(dir)
	for range 5 {
		a.Advert()
	}
	if len(*logs) != 1 {
		t.Fatalf("logs = %d, want 1: %v", len(*logs), *logs)
	}
	// Fix the manifest: advertised, and the problem state clears.
	writeManifest(t, dir, manifestJSON("b1", map[string]string{"foreman-linux-amd64": sum("a")}))
	if a.Advert() == nil {
		t.Fatal("want advert after fix")
	}
	// Breaking it again logs again.
	writeManifest(t, dir, manifestJSON("b1", map[string]string{"foreman-linux-amd64": sum("x")}))
	a.Advert()
	if len(*logs) != 2 {
		t.Fatalf("logs = %d, want 2", len(*logs))
	}
}

func TestAdvertManifestUnreadable(t *testing.T) {
	dir := t.TempDir()
	// manifest.json as a directory: read fails with a non-NotExist error.
	if err := os.Mkdir(filepath.Join(dir, "manifest.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	a, logs := newAdv(dir)
	if a.Advert() != nil || len(*logs) != 1 {
		t.Fatalf("want nil + 1 log, logs=%v", *logs)
	}
}

func TestAdvertHashCacheInvalidatesOnMtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foreman-linux-amd64")
	writeBin(t, dir, "foreman-linux-amd64", "aaaa")
	writeManifest(t, dir, manifestJSON("b1", map[string]string{"foreman-linux-amd64": sum("aaaa")}))
	a, _ := newAdv(dir)
	if a.Advert() == nil {
		t.Fatal("want advert")
	}

	// Same size + same mtime but different content: cache is trusted (proves
	// it is a cache hit rather than a re-hash).
	st, _ := os.Stat(path)
	if err := os.WriteFile(path, []byte("bbbb"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, st.ModTime(), st.ModTime()); err != nil {
		t.Fatal(err)
	}
	if a.Advert() == nil {
		t.Fatal("cache hit expected: same path+size+mtime")
	}

	// mtime change invalidates: new content no longer matches the manifest.
	later := st.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
	if a.Advert() != nil {
		t.Fatal("mtime change must re-hash and drop the mismatching platform")
	}

	// Content restored with a new mtime re-hashes and advertises again.
	if err := os.WriteFile(path, []byte("aaaa"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, later.Add(time.Hour), later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if a.Advert() == nil {
		t.Fatal("restored content should advertise again")
	}

	// Size change alone invalidates too (mtime pinned).
	if err := os.WriteFile(path, []byte("aaaaa"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, later.Add(time.Hour), later.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if a.Advert() != nil {
		t.Fatal("size change must re-hash")
	}
}

func TestRefreshHandlerAdvert(t *testing.T) {
	authz := func(r *http.Request, id *string) error { *id = "foreman:alpha"; return nil }
	upsert := func(ctx context.Context, id string, caps map[string]string) error { return nil }
	fu := &ForemanUpdate{Build: "b1", Platforms: map[string]PlatformAdvert{
		"linux-amd64": {SHA256: sum("a"), Path: "/download/foreman/foreman-linux-amd64"},
	}}
	tests := []struct {
		name       string
		advert     AdvertFunc
		wantStatus int
	}{
		{"nil advert func: old 204", nil, http.StatusNoContent},
		{"advert returns nil: 204", func() *ForemanUpdate { return nil }, http.StatusNoContent},
		{"advert present: 200", func() *ForemanUpdate { return fu }, http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := RefreshHandlerWithAdvert(authz, upsert, tt.advert)
			req := httptest.NewRequest(http.MethodPost, "/v1/foreman/capabilities", strings.NewReader(`{"capabilities":{"os":"linux"}}`))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			if tt.wantStatus == http.StatusNoContent {
				if rec.Body.Len() != 0 {
					t.Fatalf("204 with body %q", rec.Body.String())
				}
				return
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("content-type = %q", ct)
			}
			var got struct {
				FU ForemanUpdate `json:"foreman_update"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.FU.Build != "b1" || got.FU.Platforms["linux-amd64"].Path != "/download/foreman/foreman-linux-amd64" {
				t.Errorf("body = %s", rec.Body.String())
			}
		})
	}
}
