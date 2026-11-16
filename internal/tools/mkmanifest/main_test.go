package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func seed(t *testing.T, skip string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range Files {
		if n == skip {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, n), []byte("bin:"+n), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRunWritesManifest(t *testing.T) {
	dir := seed(t, "")
	if err := run([]string{"-dir", dir, "-build", "20261116T141100Z-38a6368abcde"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.Build != "20261116T141100Z-38a6368abcde" || len(m.Files) != 4 {
		t.Fatalf("bad manifest: %+v", m)
	}
	for _, n := range Files {
		sum := sha256.Sum256([]byte("bin:" + n))
		if got := m.Files[n].SHA256; got != hex.EncodeToString(sum[:]) {
			t.Errorf("%s sha = %s", n, got)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".manifest.json.tmp")); !os.IsNotExist(err) {
		t.Error("temp file left behind")
	}
}

func TestRunErrors(t *testing.T) {
	tests := []struct {
		name string
		args func(t *testing.T) []string
	}{
		{"empty build", func(t *testing.T) []string { return []string{"-dir", seed(t, "")} }},
		{"missing binary", func(t *testing.T) []string {
			return []string{"-dir", seed(t, "factory-linux-arm64"), "-build", "x"}
		}},
		{"bad flag", func(t *testing.T) []string { return []string{"-nope"} }},
		{"unwritable dir", func(t *testing.T) []string {
			d := seed(t, "")
			if err := os.Chmod(d, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(d, 0o700) })
			if os.Geteuid() == 0 {
				t.Skip("root ignores dir perms")
			}
			return []string{"-dir", d, "-build", "x"}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := run(tt.args(t)); err == nil {
				t.Fatal("want error")
			}
		})
	}
}
