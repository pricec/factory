package workerharness

// claudeConfigFileName is the generated Claude Code MCP config in the job
// dir (§8.4). It is passed to the child explicitly via --mcp-config, so
// the name is never discovered by claude's project scan; a non-`.mcp.json`
// name additionally keeps that scan from ever double-loading it.
const claudeConfigFileName = "claude-mcp.json"

// claudeHTTPMCPServer is one generated MCP server entry, verified against
// the Claude Code CLI (2.1.325): type "http" is the streamable-HTTP
// transport (the opencode "remote" analog) and headers ride the server.
type claudeHTTPMCPServer struct {
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// claudeConfig is the generated Claude Code config (§8.4). It carries only
// the MCP injection: the routed model rides the `--model` flag (no #variant
// machinery on this CLI), the headless permission policy rides the
// `--permission-mode bypassPermissions` flag (the opencode allow-all
// analog - a headless run has no one to answer a permission prompt), and
// `--strict-mcp-config` makes these servers the only MCP source (the
// isolation analog of opencode's `--standalone`).
type claudeConfig struct {
	MCPServers map[string]claudeHTTPMCPServer `json:"mcpServers"`
}

// newClaudeConfig builds the generated config from cfg (§8.4): the worker
// MCP server under the name "inbox" - Claude Code exposes its tools as
// `mcp__<server>__<tool>`, so the reporting tool is
// mcp__inbox__report_result (§8.3) - with the Authorization header
// referencing the token via Claude Code's ${VAR} env expansion so the
// per-job secret never lands in the file at rest (the {env:...} twin of
// the opencode config's reference). execChild guarantees WORKER_TOKEN is
// present in the child environment; the expansion was verified live
// against 2.1.325 (the child connects with the expanded bearer).
func newClaudeConfig(cfg Config) claudeConfig {
	return claudeConfig{MCPServers: map[string]claudeHTTPMCPServer{
		"inbox": {
			Type:    "http",
			URL:     cfg.MCPURL,
			Headers: map[string]string{"Authorization": "Bearer ${" + envWorkerToken + "}"},
		},
	}}
}
