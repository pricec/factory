package wire

import (
	"fmt"
	"strings"
)

// Touches bounds (§5.1, 2026-11-18 conflict-coordinator amendment).
const (
	// MaxTouches caps how many globs one job may declare.
	MaxTouches = 32
	// MaxTouchLen caps one glob's length in bytes.
	MaxTouchLen = 200
)

// ValidateTouches checks a job's declared footprint: at most MaxTouches
// entries, each non-empty, at most MaxTouchLen bytes, repo-relative (no
// leading `/`, no `..` segment) and free of control characters, backslashes
// and empty segments (`a//b`).
func ValidateTouches(touches []string) error {
	if len(touches) > MaxTouches {
		return fmt.Errorf("at most %d entries, got %d", MaxTouches, len(touches))
	}
	for _, g := range touches {
		if strings.TrimSpace(g) == "" {
			return fmt.Errorf("entries must be non-empty")
		}
		if len(g) > MaxTouchLen {
			return fmt.Errorf("entry %.40q...: at most %d bytes", g, MaxTouchLen)
		}
		if strings.HasPrefix(g, "/") {
			return fmt.Errorf("entry %q: must be repo-relative (no leading /)", g)
		}
		for i := 0; i < len(g); i++ {
			if g[i] < 0x20 || g[i] == 0x7f || g[i] == '\\' {
				return fmt.Errorf("entry %q: control characters and backslashes are not allowed", g)
			}
		}
		segs := strings.Split(strings.TrimSuffix(g, "/"), "/")
		for _, seg := range segs {
			if seg == ".." {
				return fmt.Errorf("entry %q: must not contain ..", g)
			}
			if seg == "" {
				return fmt.Errorf("entry %q: empty path segment", g)
			}
		}
	}
	return nil
}
