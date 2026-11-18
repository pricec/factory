package main

import (
	"os"
	"path/filepath"
	"strings"
)

// claudeLoginWarning returns a non-empty warning when any configured model
// routes to the Claude Code CLI (claude-* prefix) but no credentials are
// visible: no API key/OAuth token in the environment and no
// $HOME/.claude/.credentials.json. Heuristic and advisory only.
func claudeLoginWarning(models []string, getenv func(string) string) string {
	uses := false
	for _, m := range models {
		if strings.HasPrefix(m, "claude-") {
			uses = true
		}
	}
	if !uses {
		return ""
	}
	if getenv("ANTHROPIC_API_KEY") != "" || getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
		return ""
	}
	home := getenv("HOME")
	if home != "" {
		if _, err := os.Stat(filepath.Join(home, ".claude", ".credentials.json")); err == nil {
			return ""
		}
	}
	return "foreman: configured for Claude models but the claude CLI does not appear to be logged in; " +
		"the factory delivers its stored claude-oauth-token if one exists, and otherwise opens a Claude login task for the operator (spec §13.3)"
}
