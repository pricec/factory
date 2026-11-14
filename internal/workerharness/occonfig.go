package workerharness

import (
	"encoding/json"
	"os"
)

// remoteMCPServer is the generated remote MCP block, verified against the
// opencode V2 config shape (docs/mcp-servers: type remote, absolute url,
// string headers; oauth false for header-credential endpoints).
type remoteMCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	OAuth   bool              `json:"oauth"`
}

// mcpBlock nests the "inbox" server under mcp.servers (V2 shape; server
// names no longer sit directly under "mcp").
type mcpBlock struct {
	Servers map[string]*remoteMCPServer `json:"servers"`
}

// openCODEConfig is the generated opencode config (§8.4).
type openCODEConfig struct {
	Model string   `json:"model,omitempty"`
	MCP   mcpBlock `json:"mcp"`
}

// newOpenCODEConfig builds the generated config from cfg (§8.4): the remote
// MCP server injection plus the optional model override. The Authorization
// header references the token via {env:WORKER_TOKEN} substitution so the
// per-job secret never lands in the file at rest; execChild guarantees
// WORKER_TOKEN is present in the child environment. OAuth auto-detection is
// disabled because the endpoint authenticates with the bearer header.
func newOpenCODEConfig(cfg Config) openCODEConfig {
	return openCODEConfig{
		Model: cfg.Model,
		MCP: mcpBlock{Servers: map[string]*remoteMCPServer{
			"inbox": {
				Type:    "remote",
				URL:     cfg.MCPURL,
				Headers: map[string]string{"Authorization": "Bearer {env:WORKER_TOKEN}"},
				OAuth:   false,
			},
		}},
	}
}

// writeConfig writes the generated config at path with 0600 perms (§8.4).
func writeConfig(path string, c openCODEConfig) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}
