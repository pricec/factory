package webpush

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newUA makes a receiving "browser": a subscription plus the secrets to
// decrypt with.
func newUA(t *testing.T, endpoint string) (Subscription, *ecdh.PrivateKey, []byte) {
	t.Helper()
	k, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	auth := make([]byte, 16)
	_, _ = rand.Read(auth)
	var s Subscription
	s.Endpoint = endpoint
	s.Keys.P256dh = b64.EncodeToString(k.PublicKey().Bytes())
	s.Keys.Auth = b64.EncodeToString(auth)
	return s, k, auth
}

// decrypt is the RFC 8291 receiver side, written independently of Encrypt.
func decrypt(t *testing.T, ua *ecdh.PrivateKey, auth, body []byte) []byte {
	t.Helper()
	salt, idlen := body[:16], int(body[20])
	asPubRaw := body[21 : 21+idlen]
	ct := body[21+idlen:]
	asPub, err := ecdh.P256().NewPublicKey(asPubRaw)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := ua.ECDH(asPub)
	prkKey, _ := hkdf.Extract(sha256.New, secret, auth)
	info := "WebPush: info\x00" + string(ua.PublicKey().Bytes()) + string(asPubRaw)
	ikm, _ := hkdf.Expand(sha256.New, prkKey, info, 32)
	prk, _ := hkdf.Extract(sha256.New, ikm, salt)
	cek, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: aes128gcm\x00", 16)
	nonce, _ := hkdf.Expand(sha256.New, prk, "Content-Encoding: nonce\x00", 12)
	blk, _ := aes.NewCipher(cek)
	g, _ := cipher.NewGCM(blk)
	plain, err := g.Open(nil, nonce, ct, nil)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if plain[len(plain)-1] != 0x02 {
		t.Fatalf("missing delimiter")
	}
	return plain[:len(plain)-1]
}

func TestEncryptRoundTrip(t *testing.T) {
	sub, ua, auth := newUA(t, "https://push.example/x")
	for _, msg := range []string{"", "hello", strings.Repeat("é", 1500)} {
		body, err := Encrypt(sub, []byte(msg))
		if err != nil {
			t.Fatal(err)
		}
		if got := string(decrypt(t, ua, auth, body)); got != msg {
			t.Fatalf("round trip: got %q want %q", got, msg)
		}
	}
	if _, err := Encrypt(sub, make([]byte, 4000)); err == nil {
		t.Fatal("oversized payload accepted")
	}
}

func TestVAPIDPersistRoundTripAndJWT(t *testing.T) {
	v, err := GenerateVAPID()
	if err != nil {
		t.Fatal(err)
	}
	v2, err := ParseVAPID(v.Private())
	if err != nil {
		t.Fatal(err)
	}
	if v.Public() != v2.Public() {
		t.Fatal("public key changed across persist")
	}
	pubRaw, _ := b64.DecodeString(v.Public())
	if len(pubRaw) != 65 || pubRaw[0] != 4 {
		t.Fatalf("public key not an uncompressed point")
	}
	now := time.Unix(1_700_000_000, 0)
	h, err := v2.authHeader("https://web.push.apple.com/abc", "https://factory.example", now)
	if err != nil {
		t.Fatal(err)
	}
	rest := strings.TrimPrefix(h, "vapid t=")
	jwt, k, _ := strings.Cut(rest, ", k=")
	if k != v.Public() {
		t.Fatal("k mismatch")
	}
	parts := strings.Split(jwt, ".")
	sig, _ := b64.DecodeString(parts[2])
	if len(sig) != 64 {
		t.Fatalf("sig len %d", len(sig))
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if !ecdsa.Verify(&v.key.PublicKey, digest[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:])) {
		t.Fatal("JWT signature invalid")
	}
	claims, _ := b64.DecodeString(parts[1])
	for _, want := range []string{`"aud":"https://web.push.apple.com"`, `"sub":"https://factory.example"`, `"exp":1700043200`} {
		if !strings.Contains(string(claims), want) {
			t.Fatalf("claims %s missing %s", claims, want)
		}
	}
}

func TestParseVAPIDRejectsGarbage(t *testing.T) {
	for _, s := range []string{"", "!!", b64.EncodeToString([]byte("short"))} {
		if _, err := ParseVAPID(s); err == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}

func TestSend(t *testing.T) {
	status := http.StatusCreated
	var gotAuth, gotEnc string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotEnc = r.Header.Get("Authorization"), r.Header.Get("Content-Encoding")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	sub, ua, auth := newUA(t, srv.URL+"/s")
	v, _ := GenerateVAPID()
	s := &Sender{VAPID: v, Subject: "https://factory.example"}

	if err := s.Send(context.Background(), sub, []byte(`{"title":"t"}`)); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotAuth, "vapid t=") || gotEnc != "aes128gcm" {
		t.Fatalf("headers: %q %q", gotAuth, gotEnc)
	}
	if got := decrypt(t, ua, auth, gotBody); string(got) != `{"title":"t"}` {
		t.Fatalf("body %q", got)
	}
	for code, want := range map[int]error{404: ErrGone, 410: ErrGone} {
		status = code
		if err := s.Send(context.Background(), sub, []byte("x")); err != want {
			t.Fatalf("%d: %v", code, err)
		}
	}
	status = 500
	if err := s.Send(context.Background(), sub, []byte("x")); err == nil || err == ErrGone {
		t.Fatalf("500: %v", err)
	}
}

func TestSubscriptionValidate(t *testing.T) {
	good, _, _ := newUA(t, "https://web.push.apple.com/x")
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	bad := []func(*Subscription){
		func(s *Subscription) { s.Endpoint = "http://web.push.apple.com/x" },
		func(s *Subscription) { s.Endpoint = "" },
		func(s *Subscription) { s.Keys.P256dh = "AAAA" },
		func(s *Subscription) { s.Keys.Auth = "AAAA" },
	}
	for i, mut := range bad {
		s := good
		mut(&s)
		if s.Validate() == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}
