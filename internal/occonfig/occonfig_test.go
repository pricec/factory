package occonfig

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestParseVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		out  string
		err  error
		want Version
	}{
		{"v2 with prefix", "opencode v2.0.15\n", nil, V2},
		{"v2 bare", "2.1.0", nil, V2},
		{"v1", "1.4.3\n", nil, V1},
		{"v1 prefixed", "opencode v1.0.220", nil, V1},
		{"major ten", "10.0.0", nil, V2},
		{"probe error falls back to V1", "2.0.0", errors.New("exec: not found"), V1},
		{"empty output falls back to V1", "", nil, V1},
		{"garbage falls back to V1", "usage: opencode [options]", nil, V1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ParseVersion(tc.out, tc.err); got != tc.want {
				t.Errorf("ParseVersion(%q, %v) = %v, want %v", tc.out, tc.err, got, tc.want)
			}
		})
	}
}

// stubProbe swaps the version probe; tests using it must not be parallel.
func stubProbe(t *testing.T, fn func(bin string) (string, error)) {
	t.Helper()
	old := runVersion
	runVersion = fn
	cacheMu.Lock()
	cache = map[string]Version{}
	cacheMu.Unlock()
	t.Cleanup(func() {
		runVersion = old
		cacheMu.Lock()
		cache = map[string]Version{}
		cacheMu.Unlock()
	})
}

func TestDetect(t *testing.T) {
	calls := 0
	stubProbe(t, func(bin string) (string, error) {
		calls++
		if bin != "opencode" {
			t.Errorf("probed %q, want default opencode", bin)
		}
		return "opencode v2.0.15", nil
	})
	if got := Detect(""); got != V2 {
		t.Errorf("Detect = %v, want V2", got)
	}
	if got := Detect("opencode"); got != V2 || calls != 1 {
		t.Errorf("second Detect = %v after %d probes, want cached V2 after 1", got, calls)
	}
}

func TestDetectFallbackToV1AndRetries(t *testing.T) {
	fail := true
	calls := 0
	stubProbe(t, func(string) (string, error) {
		calls++
		if fail {
			return "", errors.New("not found")
		}
		return "2.0.1", nil
	})
	if got := Detect("opencode"); got != V1 {
		t.Fatalf("failed probe = %v, want V1 fallback", got)
	}
	fail = false
	if got := Detect("opencode"); got != V2 || calls != 2 {
		t.Errorf("after recovery = %v (%d probes), want V2 (failure not cached)", got, calls)
	}
}

func TestResolve(t *testing.T) {
	stubProbe(t, func(string) (string, error) { return "1.9.0", nil })
	if got := Resolve(V2, "opencode"); got != V2 {
		t.Errorf("explicit V2 overridden: %v", got)
	}
	if got := Resolve(Unknown, "opencode"); got != V1 {
		t.Errorf("Unknown = %v, want detected V1", got)
	}
}

var remote = Remote{Name: "inbox", URL: "http://foreman:8080/v1/mcp/worker", TokenEnv: "WORKER_TOKEN"}

func roundTrip(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func serverWant() map[string]any {
	return map[string]any{
		"type":    "remote",
		"url":     "http://foreman:8080/v1/mcp/worker",
		"headers": map[string]any{"Authorization": "Bearer {env:WORKER_TOKEN}"},
		"oauth":   false,
	}
}

func TestBuildV2(t *testing.T) {
	t.Parallel()
	got := roundTrip(t, Build(V2, "p/m", remote))
	want := map[string]any{
		"model":       "p/m",
		"permissions": []any{map[string]any{"action": "*", "resource": "*", "effect": "allow"}},
		"mcp":         map[string]any{"servers": map[string]any{"inbox": serverWant()}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("V2 config = %v\nwant %v", got, want)
	}
}

func TestBuildV1(t *testing.T) {
	t.Parallel()
	got := roundTrip(t, Build(V1, "", remote))
	want := map[string]any{
		"permission": map[string]any{
			"edit": "allow", "bash": "allow", "webfetch": "allow",
			"external_directory": "allow", "doom_loop": "allow",
		},
		"mcp": map[string]any{"inbox": serverWant()},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("V1 config = %v\nwant %v", got, want)
	}
	if _, ok := got["permissions"]; ok {
		t.Error("V1 config must not carry the V2 \"permissions\" key")
	}
	if _, ok := got["model"]; ok {
		t.Error("empty model must be omitted")
	}
}

func TestBuildUnknownIsV1(t *testing.T) {
	t.Parallel()
	if _, ok := Build(Unknown, "", remote)["permission"]; !ok {
		t.Error("Unknown version must emit V1 permission rules")
	}
}
