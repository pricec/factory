package factoryapp

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

// Speech proxy (spec §6.13): /v1/speech/* is a reverse proxy to a
// configurable speech service so the UI can offer voice input without the
// browser ever seeing the service URL or its key.
const (
	// EnvSpeechURL is the speech service base URL; unset disables speech.
	EnvSpeechURL = "FACTORY_SPEECH_URL"
	// EnvSpeechKey is the optional bearer key sent upstream.
	EnvSpeechKey = "FACTORY_SPEECH_KEY"
	// EnvSpeechSTTPath overrides the upstream STT path.
	EnvSpeechSTTPath = "FACTORY_SPEECH_STT_PATH"
	// DefaultSpeechSTTPath is the upstream path /v1/speech/stt maps to. The
	// voice service serves /api/stt today; it will move to /v1/stt.
	DefaultSpeechSTTPath = "/api/stt"
	// SpeechKeySecret is the credentials-store name consulted when
	// FACTORY_SPEECH_KEY is empty.
	SpeechKeySecret = "speech-key"

	// MaxSpeechBody caps a proxied request body.
	MaxSpeechBody = 25 << 20
	// speechRequestTimeout bounds one proxied request end to end.
	speechRequestTimeout = 10 * time.Minute
	// speechHeaderTimeout bounds the wait for upstream response headers.
	speechHeaderTimeout = 2 * time.Minute
)

// SpeechConfig configures the speech proxy. Zero URL disables it.
type SpeechConfig struct {
	URL     string // upstream base URL
	Key     string // optional bearer; empty -> credentials store "speech-key"
	STTPath string // upstream path for /v1/speech/stt; empty -> DefaultSpeechSTTPath
}

// LoadSpeechConfig reads the FACTORY_SPEECH_* environment.
func LoadSpeechConfig() SpeechConfig {
	return SpeechConfig{
		URL:     strings.TrimSpace(os.Getenv(EnvSpeechURL)),
		Key:     strings.TrimSpace(os.Getenv(EnvSpeechKey)),
		STTPath: strings.TrimSpace(os.Getenv(EnvSpeechSTTPath)),
	}
}

// speechProxy is the built proxy; nil on App when speech is disabled.
type speechProxy struct {
	base    *url.URL
	key     string
	sttPath string
	app     *App
	rp      *httputil.ReverseProxy
}

// newSpeechProxy returns nil (disabled) for an empty or invalid URL.
func newSpeechProxy(a *App, cfg SpeechConfig) *speechProxy {
	if cfg.URL == "" {
		return nil
	}
	base, err := url.Parse(cfg.URL)
	if err != nil || (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" {
		log.Printf("factoryapp: speech disabled: invalid %s", EnvSpeechURL)
		return nil
	}
	p := &speechProxy{base: base, key: cfg.Key, sttPath: cfg.STTPath, app: a}
	if p.sttPath == "" {
		p.sttPath = DefaultSpeechSTTPath
	}
	if !strings.HasPrefix(p.sttPath, "/") {
		p.sttPath = "/" + p.sttPath
	}
	p.rp = &httputil.ReverseProxy{
		Rewrite:       p.rewrite,
		FlushInterval: -1, // stream: flush every write
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: speechHeaderTimeout,
			IdleConnTimeout:       90 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var mbe *http.MaxBytesError
			switch {
			case errors.As(err, &mbe):
				writeError(w, http.StatusRequestEntityTooLarge, "too_large", "audio exceeds the speech upload cap")
			case errors.Is(err, context.DeadlineExceeded):
				writeError(w, http.StatusGatewayTimeout, "speech_timeout", "speech service timed out")
			default:
				log.Printf("factoryapp: speech proxy: %v", err)
				writeError(w, http.StatusBadGateway, "speech_unavailable", "speech service unavailable")
			}
		},
	}
	return p
}

// upstreamPath maps the /v1/speech/ remainder to the upstream path.
func (p *speechProxy) upstreamPath(rest string) string {
	if rest == "stt" {
		return p.sttPath
	}
	return "/" + rest
}

func (p *speechProxy) bearer() string {
	if p.key != "" {
		return p.key
	}
	if p.app != nil && p.app.secrets != nil {
		if v, _, err := p.app.secrets.Get(SpeechKeySecret); err == nil {
			return v
		}
	}
	return ""
}

func (p *speechProxy) rewrite(pr *httputil.ProxyRequest) {
	rest := pr.In.PathValue("path")
	pr.SetURL(p.base)
	pr.Out.URL.Path = strings.TrimRight(p.base.Path, "/") + p.upstreamPath(rest)
	pr.Out.URL.RawPath = ""
	pr.Out.URL.RawQuery = pr.In.URL.RawQuery
	pr.Out.Host = p.base.Host
	// Never forward the operator's credentials or cookies upstream.
	pr.Out.Header.Del("Authorization")
	pr.Out.Header.Del("Cookie")
	if k := p.bearer(); k != "" {
		pr.Out.Header.Set("Authorization", "Bearer "+k)
	}
}

// handleSpeechConfig serves GET /v1/speech/config: {"enabled": bool}. It
// never discloses the URL or key and is the only speech route that answers
// when speech is disabled.
func (a *App) handleSpeechConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{"enabled": a.speech != nil})
}

// handleSpeechProxy serves /v1/speech/{path...}.
func (a *App) handleSpeechProxy(w http.ResponseWriter, r *http.Request) {
	p := a.speech
	if p == nil {
		writeError(w, http.StatusNotFound, "not_found", "speech is not configured")
		return
	}
	for _, seg := range strings.Split(r.PathValue("path"), "/") {
		if seg == ".." {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid path")
			return
		}
	}
	if r.ContentLength > MaxSpeechBody {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", "audio exceeds the speech upload cap")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxSpeechBody)
	ctx, cancel := context.WithTimeout(r.Context(), speechRequestTimeout)
	defer cancel()
	p.rp.ServeHTTP(w, r.WithContext(ctx))
}
