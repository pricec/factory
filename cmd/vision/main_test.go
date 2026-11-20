package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"message":{"content":"a cat"}}`))
	}))
	img := filepath.Join(t.TempDir(), "i.png")
	if err := os.WriteFile(img, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := func(u string) func(string) string {
		return func(k string) string {
			if k == "FACTORY_VISION_URL" {
				return u
			}
			return ""
		}
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	tests := []struct {
		name     string
		args     []string
		url      string
		wantCode int
		wantOut  string
		wantErr  string
	}{
		{"ok", []string{"--prompt", "what", img}, srv.URL, 0, "a cat\n", ""},
		{"no args", nil, srv.URL, 2, "", "usage"},
		{"missing file", []string{"/no/such/file"}, srv.URL, 1, "", "vision:"},
		{"unreachable", []string{img}, deadURL, 3, "", "LAN"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run(tc.args, env(tc.url), &out, &errb)
			if code != tc.wantCode || out.String() != tc.wantOut || !strings.Contains(errb.String(), tc.wantErr) {
				t.Fatalf("code=%d out=%q err=%q", code, out.String(), errb.String())
			}
		})
	}
	srv.Close()
}
