// Package webpush is a minimal, stdlib-only Web Push sender: VAPID (RFC
// 8292, ES256 JWT) and message encryption (RFC 8291, aes128gcm). It exists
// so the factory can notify a home-screen PWA (iOS 16.4+) without a
// third-party dependency.
package webpush

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

var b64 = base64.RawURLEncoding

// Subscription is the browser's PushSubscription JSON.
type Subscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// Validate checks the subscription is structurally usable.
func (s Subscription) Validate() error {
	u, err := url.Parse(s.Endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("endpoint must be an https URL")
	}
	pub, err := b64.DecodeString(s.Keys.P256dh)
	if err != nil {
		return errors.New("keys.p256dh: not base64url")
	}
	if _, err := ecdh.P256().NewPublicKey(pub); err != nil {
		return errors.New("keys.p256dh: not a P-256 point")
	}
	if a, err := b64.DecodeString(s.Keys.Auth); err != nil || len(a) != 16 {
		return errors.New("keys.auth: want 16 base64url bytes")
	}
	return nil
}

// VAPID is the server's application-server key pair.
type VAPID struct {
	key *ecdsa.PrivateKey
}

// GenerateVAPID makes a fresh P-256 key pair.
func GenerateVAPID() (*VAPID, error) {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	return &VAPID{key: k}, nil
}

// ParseVAPID rebuilds a key pair from the base64url private scalar
// returned by Private.
func ParseVAPID(private string) (*VAPID, error) {
	raw, err := b64.DecodeString(private)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("webpush: bad VAPID private key")
	}
	ek, err := ecdh.P256().NewPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("webpush: bad VAPID private key: %w", err)
	}
	return fromECDH(ek)
}

func fromECDH(ek *ecdh.PrivateKey) (*VAPID, error) {
	pub := ek.PublicKey().Bytes()
	k := &ecdsa.PrivateKey{PublicKey: ecdsa.PublicKey{Curve: elliptic.P256()}}
	k.D = new(big.Int).SetBytes(ek.Bytes())
	k.X, k.Y = elliptic.Unmarshal(elliptic.P256(), pub) //nolint:staticcheck // no non-deprecated point decode for ecdsa.PublicKey
	if k.X == nil {
		return nil, errors.New("webpush: bad VAPID public point")
	}
	return &VAPID{key: k}, nil
}

// Private returns the base64url private scalar for persistence. Callers
// must never log it.
func (v *VAPID) Private() string {
	b, _ := v.key.Bytes()
	return b64.EncodeToString(b)
}

// Public returns the base64url uncompressed public point: the
// applicationServerKey a browser subscribes with.
func (v *VAPID) Public() string {
	b, _ := v.key.PublicKey.Bytes()
	return b64.EncodeToString(b)
}

// authHeader builds the RFC 8292 Authorization value for endpoint.
func (v *VAPID) authHeader(endpoint, subject string, now time.Time) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", err
	}
	hdr, _ := json.Marshal(map[string]string{"typ": "JWT", "alg": "ES256"})
	claims, _ := json.Marshal(map[string]any{
		"aud": u.Scheme + "://" + u.Host,
		"exp": now.Add(12 * time.Hour).Unix(),
		"sub": subject,
	})
	signing := b64.EncodeToString(hdr) + "." + b64.EncodeToString(claims)
	h := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, v.key, h[:])
	if err != nil {
		return "", err
	}
	sig := make([]byte, 64)
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return "vapid t=" + signing + "." + b64.EncodeToString(sig) + ", k=" + v.Public(), nil
}

// Encrypt encrypts payload for sub per RFC 8291 (aes128gcm, one record).
func Encrypt(sub Subscription, payload []byte) ([]byte, error) {
	if len(payload) > 3800 {
		return nil, errors.New("webpush: payload too large")
	}
	uaPubRaw, err := b64.DecodeString(sub.Keys.P256dh)
	if err != nil {
		return nil, err
	}
	authSecret, err := b64.DecodeString(sub.Keys.Auth)
	if err != nil {
		return nil, err
	}
	uaPub, err := ecdh.P256().NewPublicKey(uaPubRaw)
	if err != nil {
		return nil, err
	}
	as, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	secret, err := as.ECDH(uaPub)
	if err != nil {
		return nil, err
	}
	asPub := as.PublicKey().Bytes()
	prkKey, err := hkdf.Extract(sha256.New, secret, authSecret)
	if err != nil {
		return nil, err
	}
	keyInfo := append([]byte("WebPush: info\x00"), uaPubRaw...)
	keyInfo = append(keyInfo, asPub...)
	ikm, err := hkdf.Expand(sha256.New, prkKey, string(keyInfo), 32)
	if err != nil {
		return nil, err
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	prk, err := hkdf.Extract(sha256.New, ikm, salt)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	blk, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	plain := append(append([]byte{}, payload...), 0x02) // last-record delimiter
	var out bytes.Buffer
	out.Write(salt)
	out.Write([]byte{0, 0, 0x10, 0}) // record size 4096
	out.WriteByte(byte(len(asPub)))
	out.Write(asPub)
	out.Write(gcm.Seal(nil, nonce, plain, nil))
	return out.Bytes(), nil
}

// ErrGone means the push service says the subscription no longer exists
// (404/410): the caller should delete it.
var ErrGone = errors.New("webpush: subscription gone")

// Sender posts encrypted messages to push services.
type Sender struct {
	VAPID   *VAPID
	Subject string // mailto: or https: contact URL (RFC 8292 "sub")
	Client  *http.Client
	Now     func() time.Time
}

// Send delivers payload to sub. It returns ErrGone for 404/410.
func (s *Sender) Send(ctx context.Context, sub Subscription, payload []byte) error {
	body, err := Encrypt(sub, payload)
	if err != nil {
		return err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	auth, err := s.VAPID.authHeader(sub.Endpoint, s.Subject, now())
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sub.Endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Encoding", "aes128gcm")
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("TTL", strconv.Itoa(3600))
	req.Header.Set("Urgency", "high")
	c := s.Client
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := c.Do(req)
	if err != nil {
		// Never include the endpoint (a bearer-like capability URL) in logs.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("webpush: send: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return ErrGone
	case resp.StatusCode >= 300:
		return fmt.Errorf("webpush: push service returned %d", resp.StatusCode)
	}
	return nil
}
