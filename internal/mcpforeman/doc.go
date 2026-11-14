// Package mcpforeman exposes the foreman brain's MCP tool surface
// (spec §9.3): inbox_next, inbox_ack, post_to_factory, spawn_worker, and
// answer_worker - a stateless streamable-HTTP MCP server (go-sdk, §9.1)
// mounted on the foreman at /v1/mcp/foreman and authed by the foreman
// brain token, a foreman-scoped credential (spec §7.3). The brain never
// touches raw HTTP (spec §9.3); the foreman runtime spawns its opencode
// session per the §8.4 harness pattern with the MCP URL pointed here.
package mcpforeman
