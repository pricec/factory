package buildinfo

import (
	"runtime/debug"
	"testing"
	"time"
)

const (
	a = "20261116T141100Z-38a6368abcde"
	b = "20261116T141101Z-0123456789ab"
)

func TestNewer(t *testing.T) {
	cases := []struct {
		name      string
		cand, run string
		want      bool
	}{
		{"strictly later", b, a, true},
		{"older is a downgrade", a, b, false},
		{"identical", a, a, false},
		{"tie with different sha", "20261116T141100Z-ffffffffffff", a, false},
		{"later by a day", "20261117T000000Z-38a6368abcde", a, true},
		{"later by a year", "20271116T141100Z-38a6368abcde", a, true},
		{"candidate dev", Dev, a, false},
		{"running dev", b, Dev, false},
		{"both dev", Dev, Dev, false},
		{"candidate empty", "", a, false},
		{"running empty", b, "", false},
		{"no dash", "20261116T141101Z0123456789ab", a, false},
		{"short sha", "20261116T141101Z-0123456789a", a, false},
		{"long sha", "20261116T141101Z-0123456789abc", a, false},
		{"uppercase sha", "20261116T141101Z-0123456789AB", a, false},
		{"non-hex sha", "20261116T141101Z-0123456789xz", a, false},
		{"bad month", "20261316T141101Z-0123456789ab", a, false},
		{"missing Z", "20261116T141101-0123456789ab", a, false},
		{"local offset form", "2026-11-16T14:11:01Z-0123456789ab", a, false},
		{"garbage running", b, "garbage", false},
		{"extra suffix", b + "-dirty", a, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Newer(tc.cand, tc.run); got != tc.want {
				t.Errorf("Newer(%q, %q) = %v, want %v", tc.cand, tc.run, got, tc.want)
			}
		})
	}
}

func TestTime(t *testing.T) {
	got, ok := Time(a)
	if !ok || !got.Equal(time.Date(2026, 11, 16, 14, 11, 0, 0, time.UTC)) {
		t.Fatalf("Time(%q) = %v, %v", a, got, ok)
	}
	if _, ok := Time(Dev); ok {
		t.Fatal("Time(dev) parsed")
	}
}

func TestFormat(t *testing.T) {
	loc := time.FixedZone("x", 3600)
	got := Format(time.Date(2026, 11, 16, 15, 11, 0, 0, loc), "38A6368ABCDEF0123456")
	if got != a {
		t.Fatalf("Format = %q, want %q", got, a)
	}
	if got := Format(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "abc"); got != "20260102T030405Z-abc" {
		t.Fatalf("short rev Format = %q", got)
	}
}

func withBuildInfo(t *testing.T, id string, bi *debug.BuildInfo, ok bool) {
	t.Helper()
	oldID, oldRead := ID, readBuildInfo
	t.Cleanup(func() { ID, readBuildInfo = oldID, oldRead })
	ID = id
	readBuildInfo = func() (*debug.BuildInfo, bool) { return bi, ok }
}

func settings(kv ...string) *debug.BuildInfo {
	bi := &debug.BuildInfo{}
	for i := 0; i+1 < len(kv); i += 2 {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
	}
	return bi
}

func TestCurrent(t *testing.T) {
	full := "38a6368abcdef0123456789abcdef0123456789a"
	cases := []struct {
		name string
		id   string
		bi   *debug.BuildInfo
		ok   bool
		want string
	}{
		{"stamped id wins", a, settings("vcs.time", "2020-01-01T00:00:00Z", "vcs.revision", full), true, a},
		{"stamped id verbatim even if odd", "custom", nil, false, "custom"},
		{"vcs fallback", "", settings("vcs.time", "2026-11-16T14:11:00Z", "vcs.revision", full), true, a},
		{"vcs fallback non-UTC time", "", settings("vcs.time", "2026-11-16T15:11:00+01:00", "vcs.revision", full), true, a},
		{"no build info", "", nil, false, Dev},
		{"nil build info ok", "", nil, true, Dev},
		{"missing time", "", settings("vcs.revision", full), true, Dev},
		{"missing revision", "", settings("vcs.time", "2026-11-16T14:11:00Z"), true, Dev},
		{"short revision", "", settings("vcs.time", "2026-11-16T14:11:00Z", "vcs.revision", "abc"), true, Dev},
		{"bad time", "", settings("vcs.time", "yesterday", "vcs.revision", full), true, Dev},
		{"non-hex revision", "", settings("vcs.time", "2026-11-16T14:11:00Z", "vcs.revision", "zzzzzzzzzzzzzz"), true, Dev},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withBuildInfo(t, tc.id, tc.bi, tc.ok)
			if got := Current(); got != tc.want {
				t.Errorf("Current() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCurrentRealBuildInfo(t *testing.T) {
	// The real reader never panics and yields either "dev" or a parseable id.
	old := ID
	t.Cleanup(func() { ID = old })
	ID = ""
	got := Current()
	if got != Dev {
		if _, ok := Time(got); !ok {
			t.Fatalf("Current() = %q: neither dev nor parseable", got)
		}
	}
}
