package mcpfactory

import (
	"fmt"
	"strings"

	"github.com/pricec/factory/internal/registry"
)

// Factory-repo routing guard (spec §6.7, 2026-11-17 amendment). Work on the
// factory's own Go repo must run on the integration foreman (foreman:server),
// where its worktrees and `make ci` toolchain live. The task is free text and
// carries no structured repo field, so capability matching cannot pick that
// foreman: an unpinned factory-repo job once landed on foreman:chatfrontend.
// Instead of guessing a route from prose, create_job rejects an UNPINNED job
// whose task names the factory repo or its worktree paths and tells the caller
// to pin foreman:server. An explicit pin (any foreman) always bypasses the
// check; a pin to a different foreman only draws a warning.

// DefaultFactoryForeman is the foreman factory-repo work must be pinned to.
const DefaultFactoryForeman = "foreman:server"

// DefaultFactoryRepoMarkers are the case-insensitive substrings that mark a
// task as factory-repo work: the GitHub slug and the checkout/worktree roots.
var DefaultFactoryRepoMarkers = []string{
	"pricec/factory",
	"/home/price/work/factory",
	"/home/price/temp/factory",
}

// factoryRepoMarker returns the first marker found in task ("" if none).
func (c Config) factoryRepoMarker(task string) string {
	markers := c.FactoryRepoMarkers
	if markers == nil {
		markers = DefaultFactoryRepoMarkers
	}
	lower := strings.ToLower(task)
	for _, m := range markers {
		if m != "" && strings.Contains(lower, strings.ToLower(m)) {
			return m
		}
	}
	return ""
}

func (c Config) factoryForeman() string {
	if c.FactoryForeman != "" {
		return registry.InboxID(c.FactoryForeman)
	}
	return DefaultFactoryForeman
}

// checkFactoryRouting enforces the guard for one create_job: an error for an
// unpinned factory-repo job, a warning for one pinned to another foreman.
func (c Config) checkFactoryRouting(task, pin string) (warning string, err error) {
	marker := c.factoryRepoMarker(task)
	if marker == "" {
		return "", nil
	}
	want := c.factoryForeman()
	switch {
	case pin == "":
		return "", fmt.Errorf("factory_repo_needs_pin: the task mentions the factory repo (%q) but has no foreman pin; "+
			"factory-repo work must run on %s - resubmit with foreman=%q", marker, want, want)
	case !registry.SameForeman(pin, want):
		return fmt.Sprintf("task mentions the factory repo (%q) but is pinned to %s; factory-repo work belongs on %s", marker, pin, want), nil
	}
	return "", nil
}
