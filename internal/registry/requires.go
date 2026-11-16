// Creation-time requires validation (spec §6.7, 2026-11-15 amendment).
//
// A job's requires entries bind to capability TAG NAMES - bare ("os",
// "arch", "tools") or "name:value" ("os:linux", "arch:amd64",
// "tools:git"). Capability VALUES are not tag names and can never match:
// the matcher (§6.7) checks entries against foremen tag keys, so requires
// ["linux", "amd64"] parks the job pending forever. The 2026-11-15
// incident: the create paths accepted such values silently and two jobs
// sat pending for ~7h.
package registry

import (
	"fmt"
	"slices"
	"strings"
)

// RequiresTag returns the tag name a requires entry binds to (spec §6.7):
// the part before the first ':' in the "name:value" form, or the whole
// entry for a bare tag name.
func RequiresTag(entry string) string {
	name, _, _ := strings.Cut(entry, ":")
	return name
}

// TagKeys returns the union of capability tag keys across the foremen
// registry snapshot, sorted. A key whose value is empty counts as absent
// (§6.7 Has) and is skipped: a requires entry naming it could never match.
func TagKeys(foremen []Foreman) []string {
	var keys []string
	for _, f := range foremen {
		for tag, v := range f.Capabilities {
			if v != "" {
				keys = append(keys, tag)
			}
		}
	}
	slices.Sort(keys)
	return slices.Compact(keys)
}

// UnknownRequiresError is the creation-time requires validation failure
// (spec §6.7): the entries name no registered foreman tag key, so no
// foreman can ever match the job. HTTP maps it to 400 unknown_requires;
// the MCP create_job tool returns it as a tool error.
type UnknownRequiresError struct {
	// Unknown are the offending entries (deduplicated, first-seen order).
	Unknown []string
	// Known is the union of registered tag keys they were checked against.
	Known []string
	// Hints are unknown entries that look like registered tag VALUES - the
	// values-not-names mistake ("linux" when some foreman has os=linux).
	Hints []string
}

func (e *UnknownRequiresError) Error() string {
	msg := fmt.Sprintf("unknown requires %v: match no registered foreman tag key (known: %v); requires entries are tag names - bare (os, arch, tools) or name:value (os:linux, arch:amd64, tools:git)",
		e.Unknown, e.Known)
	if len(e.Hints) > 0 {
		msg += fmt.Sprintf("; %v look like capability values, not tag names", e.Hints)
	}
	return msg
}

// ValidateRequires checks a job's requires entries at creation time (spec
// §6.7, 2026-11-15 amendment): every entry's tag name must be in the union
// of registered foremen tag keys, else no foreman can ever match and the
// job would park pending silently. With no foremen registered nothing is
// known yet - creation succeeds with a loud warning instead (the pending
// picker dispatches once a matching foreman arrives). Empty requires
// always validates. A nil warning slice means no warnings.
func ValidateRequires(requires []string, foremen []Foreman) (warnings []string, err error) {
	if len(requires) == 0 {
		return nil, nil
	}
	keys := TagKeys(foremen)
	if len(keys) == 0 {
		return []string{
			"requires not validated: no foremen registered yet - entries must be tag names (bare like os, arch, tools, or name:value like os:linux, arch:amd64, tools:git), never capability values (linux, amd64)",
		}, nil
	}
	keySet := make(map[string]bool, len(keys))
	for _, k := range keys {
		keySet[k] = true
	}
	valueSet := make(map[string]bool)
	for _, f := range foremen {
		for _, v := range f.Capabilities {
			for _, part := range strings.Split(v, ",") {
				valueSet[strings.TrimSpace(part)] = true
			}
		}
	}
	var unknown, hints []string
	for _, entry := range requires {
		if keySet[RequiresTag(entry)] {
			continue
		}
		if !slices.Contains(unknown, entry) {
			unknown = append(unknown, entry)
		}
		if valueSet[entry] && !slices.Contains(hints, entry) {
			hints = append(hints, entry)
		}
	}
	if len(unknown) == 0 {
		return nil, nil
	}
	return nil, &UnknownRequiresError{Unknown: unknown, Known: keys, Hints: hints}
}
