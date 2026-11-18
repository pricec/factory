// Package occonfig generates the opencode project config (opencode.json)
// in the shape the installed opencode understands, and detects which
// shape that is.
//
// opencode V1 and V2 read different config schemas and each refuses the
// other's: V1 rejects V2's top-level "permissions" array ("V2 permissions
// are not supported by OpenCode V1") and expects "permission" rules and
// servers directly under "mcp"; V2 wants "permissions" rules and servers
// under "mcp.servers". Fleet hosts run a mix, so the foreman picks the
// shape per host. When the version cannot be determined it falls back to
// V1, which every host can read.
package occonfig

import (
	"context"
	"os/exec"
	"regexp"
	"strconv"
	"sync"
	"time"
)

// Version is the opencode config schema generation.
type Version int

const (
	// Unknown means "not yet detected"; Resolve turns it into V1 or V2.
	Unknown Version = iota
	// V1 is the legacy schema ("permission" object, mcp.<name>).
	V1
	// V2 is the new schema ("permissions" array, mcp.servers.<name>).
	V2
)

func (v Version) String() string {
	switch v {
	case V1:
		return "V1"
	case V2:
		return "V2"
	}
	return "unknown"
}

// versionTimeout bounds the `<bin> --version` probe.
const versionTimeout = 10 * time.Second

var versionRE = regexp.MustCompile(`(\d+)\.\d+(?:\.\d+)?`)

// ParseVersion maps `<bin> --version` output to a schema generation. A
// failed probe or unparseable output yields V1 (the safe fallback): major
// version 2 or higher is V2, anything else V1.
func ParseVersion(out string, err error) Version {
	if err != nil {
		return V1
	}
	m := versionRE.FindStringSubmatch(out)
	if m == nil {
		return V1
	}
	if major, convErr := strconv.Atoi(m[1]); convErr == nil && major >= 2 {
		return V2
	}
	return V1
}

// runVersion runs `<bin> --version`. Test seam.
var runVersion = func(bin string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--version").Output()
	return string(out), err
}

var (
	cacheMu sync.Mutex
	cache   = map[string]Version{}
)

// Detect reports the config schema generation of the opencode binary bin
// ("" means "opencode" on PATH) from `bin --version`. Successful probes
// are cached per binary for the life of the process; a failed probe
// answers V1 and is retried next time (the binary may appear later).
func Detect(bin string) Version {
	if bin == "" {
		bin = "opencode"
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if v, ok := cache[bin]; ok {
		return v
	}
	out, err := runVersion(bin)
	v := ParseVersion(out, err)
	if err == nil {
		cache[bin] = v
	}
	return v
}

// Resolve returns v, detecting from bin when v is Unknown.
func Resolve(v Version, bin string) Version {
	if v == V1 || v == V2 {
		return v
	}
	return Detect(bin)
}

// Remote describes the one remote MCP server injected into the child. The
// bearer rides an {env:TokenEnv} substitution so the secret never lands in
// the file at rest.
type Remote struct {
	Name     string
	URL      string
	TokenEnv string
}

func (r Remote) server() map[string]any {
	return map[string]any{
		"type":    "remote",
		"url":     r.URL,
		"headers": map[string]string{"Authorization": "Bearer {env:" + r.TokenEnv + "}"},
		"oauth":   false, // header-credential endpoint: no OAuth auto-detection
	}
}

// Build returns the opencode.json document for schema v (Unknown is
// treated as V1). Both shapes carry the same policy: an allow-all for the
// headless child. A headless `opencode run` has no client to answer an
// "ask", so any prompt auto-declines and aborts the session mid-flight.
func Build(v Version, model string, remote Remote) map[string]any {
	doc := map[string]any{}
	if model != "" {
		doc["model"] = model
	}
	if v == V2 {
		doc["mcp"] = map[string]any{"servers": map[string]any{remote.Name: remote.server()}}
		doc["permissions"] = []map[string]string{{"action": "*", "resource": "*", "effect": "allow"}}
		return doc
	}
	doc["mcp"] = map[string]any{remote.Name: remote.server()}
	doc["permission"] = V1AllowAll()
	return doc
}

// V1AllowAll is the V1 spelling of the V2 `*`/`*` allow rule. Every
// permission key V1 gates on is listed explicitly (rather than a "*"
// wildcard, which older V1 schemas reject): edit, bash, webfetch,
// external_directory (reads outside the workdir - the original abort),
// and doom_loop.
func V1AllowAll() map[string]string {
	return map[string]string{
		"edit":               "allow",
		"bash":               "allow",
		"webfetch":           "allow",
		"external_directory": "allow",
		"doom_loop":          "allow",
	}
}
