package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFromEnv(t *testing.T) {
	tests := []struct {
		name, url, model, wantURL, wantModel string
	}{
		{"defaults", "", "", DefaultURL, DefaultModel},
		{"override", " http://x:1 ", "m", "http://x:1", "m"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := FromEnv(func(k string) string {
				if k == "FACTORY_VISION_URL" {
					return tc.url
				}
				return tc.model
			})
			if c.URL != tc.wantURL || c.Model != tc.wantModel {
				t.Fatalf("got %q %q", c.URL, c.Model)
			}
		})
	}
}

func TestDescribe(t *testing.T) {
	img := []byte("fake-image-bytes")
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/chat" || r.Method != http.MethodPost {
			t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"message":{"role":"assistant","content":"  a red square \n"}}`))
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL + "/", Model: "m1"}
	out, err := c.Describe(context.Background(), img, "what?")
	if err != nil {
		t.Fatal(err)
	}
	if out != "a red square" {
		t.Fatalf("out=%q", out)
	}
	if got.Model != "m1" || got.Stream || len(got.Messages) != 1 ||
		got.Messages[0].Content != "what?" ||
		len(got.Messages[0].Images) != 1 ||
		got.Messages[0].Images[0] != base64.StdEncoding.EncodeToString(img) {
		t.Fatalf("bad request body: %+v", got)
	}
}

func TestDescribeDefaultPrompt(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"message":{"content":"ok"}}`))
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, Model: "m"}
	if _, err := c.Describe(context.Background(), []byte("x"), " "); err != nil {
		t.Fatal(err)
	}
	if got.Messages[0].Content != DefaultPrompt {
		t.Fatalf("prompt=%q", got.Messages[0].Content)
	}
}

func TestDescribeErrors(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"http 404 model missing", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"model not found"}`))
		}, "model not found"},
		{"200 with error", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"error":"boom"}`))
		}, "boom"},
		{"bad json", func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`<html>`))
		}, "bad response JSON"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			_, err := (&Client{URL: srv.URL, Model: "m"}).Describe(context.Background(), []byte("x"), "p")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want %q", err, tc.want)
			}
			if errors.Is(err, ErrUnreachable) {
				t.Fatal("must not be ErrUnreachable")
			}
		})
	}
}

func TestDescribeUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	u := srv.URL
	srv.Close() // nothing listens any more
	_, err := (&Client{URL: u, Model: "m"}).Describe(context.Background(), []byte("x"), "p")
	if !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "LAN") {
		t.Fatalf("err=%v", err)
	}
}

func TestDescribeTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(time.Second):
		}
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, Model: "m", Timeout: 100 * time.Millisecond}
	_, err := c.Describe(context.Background(), []byte("x"), "p")
	if err == nil || errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "within") {
		t.Fatalf("err=%v", err)
	}
}

func TestDescribeBadInput(t *testing.T) {
	if _, err := (&Client{URL: "http://x"}).Describe(context.Background(), nil, "p"); err == nil {
		t.Fatal("want empty image error")
	}
	if _, err := (&Client{URL: "nope"}).Describe(context.Background(), []byte("x"), "p"); err == nil {
		t.Fatal("want bad URL error")
	}
}

func TestLoadImage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.png")
	if err := os.WriteFile(p, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(dir, "e.png")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ok" {
			_, _ = w.Write([]byte("remote"))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	ctx := context.Background()
	tests := []struct {
		src     string
		want    string
		wantErr bool
	}{
		{p, "png", false},
		{srv.URL + "/ok", "remote", false},
		{srv.URL + "/missing", "", true},
		{filepath.Join(dir, "none"), "", true},
		{empty, "", true},
	}
	for _, tc := range tests {
		b, err := LoadImage(ctx, tc.src, nil)
		if (err != nil) != tc.wantErr || string(b) != tc.want {
			t.Errorf("%s: b=%q err=%v", tc.src, b, err)
		}
	}
}

func TestLockSerializes(t *testing.T) {
	p := filepath.Join(t.TempDir(), "l")
	un, err := Lock(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := Lock(ctx, p); err == nil {
		t.Fatal("second lock should time out while first held")
	}
	un()
	un2, err := Lock(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	un2()
}

// TestLive makes ONE real call to the LAN endpoint. Opt-in:
// FACTORY_VISION_LIVE=1 go test ./internal/vision -run Live -v
func TestLive(t *testing.T) {
	if os.Getenv("FACTORY_VISION_LIVE") != "1" {
		t.Skip("set FACTORY_VISION_LIVE=1 to call the real vision endpoint")
	}
	im := image.NewRGBA(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			im.Set(x, y, color.RGBA{R: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, im); err != nil {
		t.Fatal(err)
	}
	c := FromEnv(os.Getenv)
	out, err := c.Describe(context.Background(), buf.Bytes(), "What single color fills this image? Answer in one word.")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model answer: %q", out)
	if !strings.Contains(strings.ToLower(out), "red") {
		t.Errorf("expected 'red' in answer, got %q", out)
	}
}
