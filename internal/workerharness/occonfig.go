package workerharness

import (
	"encoding/json"
	"os"

	"github.com/pricec/factory/internal/occonfig"
)

// newOpenCODEConfig builds the generated config from cfg (§8.4): the remote
// MCP server injection plus the optional model override, in the schema of
// the installed opencode (V1 "permission" rules or V2 "permissions"; see
// internal/occonfig - a detection failure falls back to V1). The
// Authorization header references the token via {env:WORKER_TOKEN}
// substitution so the per-job secret never lands in the file at rest;
// execChild guarantees WORKER_TOKEN is present in the child environment.
// OAuth auto-detection is disabled because the endpoint authenticates with
// the bearer header. The policy is an allow-all either way: a headless
// `opencode run` has no client to answer an "ask", so any prompt is
// auto-declined and the whole session aborts mid-flight.
func newOpenCODEConfig(cfg Config) map[string]any {
	v := cfg.OpenCODEVersion
	if v == occonfig.Unknown && cfg.OpenCODEBin != "" && cfg.OpenCODEBin != "opencode" {
		// An explicitly configured binary (wrapper/test double) is not
		// probed, like supportsStandalone: it may not take --version.
		v = occonfig.V2
	}
	v = occonfig.Resolve(v, cfg.OpenCODEBin)
	return occonfig.Build(v, cfg.Model, occonfig.Remote{
		Name:     "inbox",
		URL:      cfg.MCPURL,
		TokenEnv: envWorkerToken,
	})
}

// writeConfig writes a generated config (opencode or claude shape, §8.4)
// at path with 0600 perms.
func writeConfig(path string, c any) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}
