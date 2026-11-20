// Package vision is a thin client for the LAN vision model served by ollama
// (/api/chat, stream=false). It is LAN-only; see docs/vision.md.
package vision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

const (
	DefaultURL     = "http://192.168.1.147:11435"
	DefaultModel   = "qwen2.5vl:7b"
	DefaultPrompt  = "Describe this image. Transcribe any visible text verbatim."
	DefaultTimeout = 120 * time.Second
	// MaxImageBytes bounds the image read or downloaded (before base64).
	MaxImageBytes = 20 << 20
)

// Client talks to one ollama endpoint. Calls should be serial: the GPU is
// shared, so do not run Describe concurrently (the vision CLI also takes a
// per-host lock).
type Client struct {
	URL     string // base URL, no trailing path
	Model   string
	HTTP    *http.Client // nil: a client with DefaultTimeout
	Timeout time.Duration
}

// FromEnv builds a Client from FACTORY_VISION_URL / FACTORY_VISION_MODEL.
func FromEnv(getenv func(string) string) *Client {
	c := &Client{URL: DefaultURL, Model: DefaultModel, Timeout: DefaultTimeout}
	if v := strings.TrimSpace(getenv("FACTORY_VISION_URL")); v != "" {
		c.URL = v
	}
	if v := strings.TrimSpace(getenv("FACTORY_VISION_MODEL")); v != "" {
		c.Model = v
	}
	return c
}

// ErrUnreachable wraps connection-level failures (not on the LAN, host down).
var ErrUnreachable = errors.New("vision: endpoint unreachable")

type message struct {
	Role    string   `json:"role"`
	Content string   `json:"content"`
	Images  []string `json:"images,omitempty"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Stream   bool      `json:"stream"`
}

type chatResponse struct {
	Message message `json:"message"`
	Error   string  `json:"error"`
}

// Describe sends one image and prompt and returns the model's text answer.
func (c *Client) Describe(ctx context.Context, image []byte, prompt string) (string, error) {
	if len(image) == 0 {
		return "", errors.New("vision: empty image")
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = DefaultPrompt
	}
	base, err := url.Parse(c.URL)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("vision: bad endpoint URL %q", c.URL)
	}
	body, err := json.Marshal(chatRequest{
		Model: c.Model,
		Messages: []message{{
			Role: "user", Content: prompt,
			Images: []string{base64.StdEncoding.EncodeToString(image)},
		}},
	})
	if err != nil {
		return "", err
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	endpoint := strings.TrimRight(c.URL, "/") + "/api/chat"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("vision: no answer from %s within %s (model busy, or foreman not on the LAN?): %w", c.URL, timeout, err)
		}
		return "", fmt.Errorf("%w: %s: %v (the vision model is LAN-only; is this foreman on the LAN?)", ErrUnreachable, c.URL, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", fmt.Errorf("vision: read response: %w", err)
	}
	var out chatResponse
	jerr := json.Unmarshal(raw, &out)
	if resp.StatusCode != http.StatusOK {
		msg := out.Error
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
			if len(msg) > 300 {
				msg = msg[:300]
			}
		}
		return "", fmt.Errorf("vision: %s returned %s: %s", c.URL, resp.Status, msg)
	}
	if jerr != nil {
		return "", fmt.Errorf("vision: bad response JSON: %w", jerr)
	}
	if out.Error != "" {
		return "", fmt.Errorf("vision: model error: %s", out.Error)
	}
	return strings.TrimSpace(out.Message.Content), nil
}

// LoadImage reads an image from a file path or an http(s) URL.
func LoadImage(ctx context.Context, src string, hc *http.Client) ([]byte, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		if hc == nil {
			hc = &http.Client{Timeout: 60 * time.Second}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if err != nil {
			return nil, err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("vision: fetch image: %w", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("vision: fetch image %s: %s", src, resp.Status)
		}
		return readLimited(resp.Body)
	}
	f, err := os.Open(src)
	if err != nil {
		return nil, fmt.Errorf("vision: %w", err)
	}
	defer f.Close()
	return readLimited(f)
}

func readLimited(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxImageBytes+1))
	if err != nil {
		return nil, fmt.Errorf("vision: read image: %w", err)
	}
	if len(b) > MaxImageBytes {
		return nil, fmt.Errorf("vision: image larger than %d MiB", MaxImageBytes>>20)
	}
	if len(b) == 0 {
		return nil, errors.New("vision: image is empty")
	}
	return b, nil
}

// Lock takes an exclusive per-host advisory lock so concurrent jobs on one
// machine queue instead of hitting the shared GPU in parallel. It returns
// the release func. Cross-host serialization is up to ollama's own queue.
func Lock(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return nil, fmt.Errorf("vision: lock: %w", err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if err != syscall.EWOULDBLOCK {
			f.Close()
			return nil, fmt.Errorf("vision: lock: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("vision: waiting for another vision call: %w", ctx.Err())
		case <-time.After(250 * time.Millisecond):
		}
	}
}
