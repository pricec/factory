package factoryapp_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pricec/factory/internal/factoryapp"
)

// speechUpstream records what the proxy forwarded.
type speechUpstream struct {
	mu                    sync.Mutex
	path, query, ct, auth string
	cookie                string
	body                  []byte
	hits                  int
}

func newSpeechUpstream(t *testing.T) (*speechUpstream, *httptest.Server) {
	u := &speechUpstream{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		u.mu.Lock()
		u.hits++
		u.path, u.query, u.ct = r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type")
		u.auth, u.cookie, u.body = r.Header.Get("Authorization"), r.Header.Get("Cookie"), b
		u.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello world"}`))
	}))
	t.Cleanup(ts.Close)
	return u, ts
}

func speechEnv(t *testing.T, cfg factoryapp.SpeechConfig) *testEnv {
	return newEnv(t, func(c *factoryapp.Config) { c.Speech = cfg })
}

func TestSpeechProxyForwards(t *testing.T) {
	up, ts := newSpeechUpstream(t)
	e := speechEnv(t, factoryapp.SpeechConfig{URL: ts.URL, Key: "s3cret"})

	req, _ := http.NewRequest("POST", e.ts.URL+"/v1/speech/stt?language=en", bytes.NewReader([]byte("RIFFaudio")))
	req.Header.Set("Content-Type", "audio/mp4")
	req.Header.Set("Authorization", "Bearer browser-token")
	req.Header.Set("Cookie", "a=b")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "hello world") {
		t.Fatalf("status %d body %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), "s3cret") || resp.Header.Get("Authorization") != "" {
		t.Error("key leaked to the browser")
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.path != "/api/stt" || up.query != "language=en" {
		t.Errorf("upstream got %s?%s, want /api/stt?language=en", up.path, up.query)
	}
	if up.ct != "audio/mp4" || string(up.body) != "RIFFaudio" {
		t.Errorf("content-type/body not forwarded: %q %q", up.ct, up.body)
	}
	if up.auth != "Bearer s3cret" {
		t.Errorf("upstream Authorization = %q, want the server-held key (not the browser's)", up.auth)
	}
	if up.cookie != "" {
		t.Errorf("cookie forwarded: %q", up.cookie)
	}
}

func TestSpeechProxySTTPathAndNoKey(t *testing.T) {
	up, ts := newSpeechUpstream(t)
	e := speechEnv(t, factoryapp.SpeechConfig{URL: ts.URL, STTPath: "/v1/stt"})
	resp, err := http.Post(e.ts.URL+"/v1/speech/stt", "audio/webm", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.path != "/v1/stt" {
		t.Errorf("upstream path = %q, want /v1/stt", up.path)
	}
	if up.auth != "" {
		t.Errorf("Authorization = %q, want none without a key", up.auth)
	}
}

func TestSpeechKeyFromSecretsStore(t *testing.T) {
	up, ts := newSpeechUpstream(t)
	e := speechEnv(t, factoryapp.SpeechConfig{URL: ts.URL})
	if _, err := e.a.Secrets().Put(factoryapp.SpeechKeySecret, "stored-key", ""); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(e.ts.URL+"/v1/speech/stt", "audio/webm", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.auth != "Bearer stored-key" {
		t.Errorf("Authorization = %q, want the stored key", up.auth)
	}
}

func TestSpeechDisabled(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ method, path string }{
		{"POST", "/v1/speech/stt"}, {"GET", "/v1/speech/healthz"},
	} {
		req, _ := http.NewRequest(c.method, e.ts.URL+c.path, strings.NewReader("x"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404 when unset", c.method, c.path, resp.StatusCode)
		}
	}
	var cfg struct {
		Enabled bool `json:"enabled"`
	}
	if st := e.do("GET", "/v1/speech/config", "", nil, &cfg); st != 200 || cfg.Enabled {
		t.Errorf("config = %d %+v, want 200 enabled=false", st, cfg)
	}
}

func TestSpeechConfigEnabledHidesSecrets(t *testing.T) {
	_, ts := newSpeechUpstream(t)
	e := speechEnv(t, factoryapp.SpeechConfig{URL: ts.URL, Key: "s3cret"})
	resp, err := http.Get(e.ts.URL + "/v1/speech/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), `"enabled":true`) || strings.Contains(string(b), "s3cret") || strings.Contains(string(b), ts.URL) {
		t.Errorf("config body = %s", b)
	}
}

func TestSpeechBodyCap(t *testing.T) {
	up, ts := newSpeechUpstream(t)
	e := speechEnv(t, factoryapp.SpeechConfig{URL: ts.URL})
	big := bytes.NewReader(make([]byte, factoryapp.MaxSpeechBody+1))
	// Known length: rejected before any upstream hit.
	resp, err := http.Post(e.ts.URL+"/v1/speech/stt", "audio/webm", big)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
	// Unknown length (chunked): cut off mid-stream.
	req, _ := http.NewRequest("POST", e.ts.URL+"/v1/speech/stt", io.NopCloser(bytes.NewReader(make([]byte, factoryapp.MaxSpeechBody+1))))
	req.ContentLength = -1
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("chunked oversize body accepted")
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	if up.hits != 0 && len(up.body) > factoryapp.MaxSpeechBody {
		t.Errorf("upstream received %d bytes, over the cap", len(up.body))
	}
}

func TestSpeechUpstreamDown(t *testing.T) {
	_, ts := newSpeechUpstream(t)
	url := ts.URL
	ts.Close()
	e := speechEnv(t, factoryapp.SpeechConfig{URL: url})
	resp, err := http.Post(e.ts.URL+"/v1/speech/stt", "audio/webm", strings.NewReader("x"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
}
