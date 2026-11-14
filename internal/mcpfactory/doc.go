// Package mcpfactory exposes the factory planner's MCP tool surface (spec
// §9.4): list_foremen, list_jobs, create_job, read_result,
// message_foreman, and notify_operator, a streamable-HTTP MCP server
// mounted on the factory at /v1/mcp/factory, authed by scope-free planner
// credentials (identity factory:planner).
package mcpfactory
