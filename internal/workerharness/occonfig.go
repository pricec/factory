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

// permissionRule is one opencode V2 permission rule (opencode
// docs/permissions): allow/deny/ask for one action on one resource.
type permissionRule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// headlessPermissions pins the generated child's permission policy. A
// headless `opencode run` has no client to answer an "ask": any prompt is
// auto-declined and the whole session aborts mid-flight (exit status 1,
// work already done discarded). The child runs with the service user's
// authority either way, so an explicit allow-all is the only sound policy;
// gating machine-run children is the enclosing service's job.
var headlessPermissions = []permissionRule{
	{Action: "*", Resource: "*", Effect: "allow"},
}

// openCODEConfig is the generated opencode config (§8.4).
type openCODEConfig struct {
	Model       string           `json:"model,omitempty"`
	MCP         mcpBlock         `json:"mcp"`
	Permissions []permissionRule `json:"permissions"`
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
		Permissions: headlessPermissions,
	}
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
