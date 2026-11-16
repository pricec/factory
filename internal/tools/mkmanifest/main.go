// Command mkmanifest writes <dir>/manifest.json for `make release`
// (spec §8.4 release manifest): the build id plus the SHA-256 of each
// release binary. Pure stdlib; no jq/python needed at release time.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Files are the release binaries recorded in the manifest.
var Files = []string{
	"foreman-linux-amd64",
	"foreman-linux-arm64",
	"factory-linux-amd64",
	"factory-linux-arm64",
}

type fileEntry struct {
	SHA256 string `json:"sha256"`
}

type manifest struct {
	Build string               `json:"build"`
	Files map[string]fileEntry `json:"files"`
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// generate builds the manifest for dir and writes it atomically.
func generate(dir, build string) error {
	if build == "" {
		return fmt.Errorf("-build must not be empty")
	}
	m := manifest{Build: build, Files: map[string]fileEntry{}}
	for _, name := range Files {
		sum, err := hashFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("hash %s: %w", name, err)
		}
		m.Files[name] = fileEntry{SHA256: sum}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := filepath.Join(dir, ".manifest.json.tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "manifest.json"))
}

func run(args []string) error {
	fs := flag.NewFlagSet("mkmanifest", flag.ContinueOnError)
	dir := fs.String("dir", "dist", "directory holding the release binaries")
	build := fs.String("build", "", "build id (<UTC commit time>-<12-char sha>)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return generate(*dir, *build)
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mkmanifest:", err)
		os.Exit(1)
	}
}
