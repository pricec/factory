// Package buildinfo identifies the running binary's build (auto-update v1
// contract C1, spec §12): a sortable build id "<UTC commit time
// YYYYMMDDTHHMMSSZ>-<12-char git sha>" stamped by the Makefile via
// -ldflags "-X github.com/pricec/factory/internal/buildinfo.ID=$(BUILD_ID)".
package buildinfo

import (
	"runtime/debug"
	"strings"
	"time"
)

// ID is the build id stamped at link time; empty in un-stamped builds
// (go test, go run, plain go build).
var ID string

// Dev is the id of a build that carries no usable version information. It
// never parses, so Newer treats it as incomparable (never updates to or
// from it by comparison).
const Dev = "dev"

// timeLayout is the build id's timestamp prefix layout (UTC).
const timeLayout = "20060102T150405Z"

// shaLen is the length of the build id's git sha suffix.
const shaLen = 12

// readBuildInfo is the debug.ReadBuildInfo seam (tests replace it).
var readBuildInfo = debug.ReadBuildInfo

// Current returns the running build id: ID when stamped; else the module's
// embedded VCS stamp (vcs.time + vcs.revision) rendered in the same
// format; else "dev".
func Current() string {
	if ID != "" {
		return ID
	}
	bi, ok := readBuildInfo()
	if !ok || bi == nil {
		return Dev
	}
	var vcsTime, rev string
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.time":
			vcsTime = s.Value
		case "vcs.revision":
			rev = s.Value
		}
	}
	if vcsTime == "" || len(rev) < shaLen {
		return Dev
	}
	t, err := time.Parse(time.RFC3339, vcsTime)
	if err != nil {
		return Dev
	}
	id := Format(t, rev)
	if _, ok := parse(id); !ok {
		return Dev
	}
	return id
}

// Format renders a build id from a commit time and a git revision (the
// revision is truncated to 12 characters).
func Format(commit time.Time, rev string) string {
	if len(rev) > shaLen {
		rev = rev[:shaLen]
	}
	return commit.UTC().Format(timeLayout) + "-" + strings.ToLower(rev)
}

// Time returns the build id's commit timestamp; ok is false when the id
// does not parse (including "dev").
func Time(id string) (time.Time, bool) { return parse(id) }

// parse validates a build id and returns its timestamp.
func parse(id string) (time.Time, bool) {
	prefix, sha, found := strings.Cut(id, "-")
	if !found || len(prefix) != len(timeLayout) || len(sha) != shaLen {
		return time.Time{}, false
	}
	for _, c := range sha {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return time.Time{}, false
		}
	}
	t, err := time.Parse(timeLayout, prefix)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Newer reports whether candidate is strictly newer than running: true
// only when both ids parse and candidate's timestamp is strictly later.
// Ties (same commit time, even with a different sha), "dev", and
// unparseable ids are false - never downgrade, never loop.
func Newer(candidate, running string) bool {
	ct, ok := parse(candidate)
	if !ok {
		return false
	}
	rt, ok := parse(running)
	if !ok {
		return false
	}
	return ct.After(rt)
}
