package factoryapp_test

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/pricec/factory/internal/factoryapp"
)

func TestCapabilitiesRefreshAdvertsForemanUpdate(t *testing.T) {
	dl := t.TempDir()
	e := newEnv(t, func(c *factoryapp.Config) { c.DownloadsDir = dl })
	bearer := e.pair("alpha", nil)
	refresh := func(out any) int {
		return e.do("POST", "/v1/foreman/capabilities", bearer, map[string]any{
			"capabilities": map[string]string{"build": "old"},
		}, out)
	}

	// No manifest: legacy 204.
	if st := refresh(nil); st != http.StatusNoContent {
		t.Fatalf("no manifest: status %d, want 204", st)
	}

	bin := []byte("foreman-bin")
	if err := os.WriteFile(filepath.Join(dl, "foreman-linux-amd64"), bin, 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(bin)
	hexsum := hex.EncodeToString(sum[:])
	man := `{"build":"20261116T141100Z-38a6368abcde","files":{"foreman-linux-amd64":{"sha256":"` + hexsum + `"}}}`
	if err := os.WriteFile(filepath.Join(dl, "manifest.json"), []byte(man), 0o644); err != nil {
		t.Fatal(err)
	}
	var got struct {
		FU struct {
			Build     string `json:"build"`
			Platforms map[string]struct {
				SHA256 string `json:"sha256"`
				Path   string `json:"path"`
			} `json:"platforms"`
		} `json:"foreman_update"`
	}
	if st := refresh(&got); st != http.StatusOK {
		t.Fatalf("valid manifest: status %d, want 200", st)
	}
	p := got.FU.Platforms["linux-amd64"]
	if got.FU.Build != "20261116T141100Z-38a6368abcde" || p.SHA256 != hexsum || p.Path != "/download/foreman/foreman-linux-amd64" {
		t.Fatalf("advert = %+v", got)
	}

	// Corrupt the binary: advert withdrawn, back to 204.
	if err := os.WriteFile(filepath.Join(dl, "foreman-linux-amd64"), []byte("tampered-and-longer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if st := refresh(nil); st != http.StatusNoContent {
		t.Fatalf("sha mismatch: status %d, want 204", st)
	}
}
