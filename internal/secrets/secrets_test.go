package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), FileName)
	s, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	return s, p
}

func TestPutValidation(t *testing.T) {
	tests := []struct {
		name, value, env string
		wantEnv          string
		bad              bool
	}{
		{"gh-token", "v", "", "SECRET_GH_TOKEN", false},
		{"a", "v", "MY_ENV_1", "MY_ENV_1", false},
		{"0abc", "v", "", "SECRET_0ABC", false},
		{strings.Repeat("a", 64), "v", "", "SECRET_" + strings.Repeat("A", 64), false},
		{strings.Repeat("a", 65), "v", "", "", true},
		{"", "v", "", "", true},
		{"-lead", "v", "", "", true},
		{"Upper", "v", "", "", true},
		{"un_der", "v", "", "", true},
		{"has space", "v", "", "", true},
		{"ok", "", "", "", true},
		{"ok", "v", "lower", "", true},
		{"ok", "v", "BAD-DASH", "", true},
		{"ok", "v", "BAD ENV", "", true},
		{"ok", strings.Repeat("x", MaxValueBytes), "", "SECRET_OK", false},
		{"ok", strings.Repeat("x", MaxValueBytes+1), "", "", true},
	}
	for _, tc := range tests {
		s, _ := open(t)
		m, err := s.Put(tc.name, tc.value, tc.env)
		if tc.bad {
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Put(%.20q,..,%q) err=%v, want ErrInvalid", tc.name, tc.env, err)
			} else if tc.value != "" && strings.Contains(err.Error(), tc.value) && len(tc.value) > 1 {
				t.Errorf("error leaks value")
			}
			continue
		}
		if err != nil || m.EnvVar != tc.wantEnv {
			t.Errorf("Put(%.20q) = %+v, %v; want env %q", tc.name, m, err, tc.wantEnv)
		}
	}
}

func TestRoundTripUpsertDelete(t *testing.T) {
	s, _ := open(t)
	if _, err := s.Put("tok", "one", ""); err != nil {
		t.Fatal(err)
	}
	if v, m, err := s.Get("tok"); err != nil || v != "one" || m.EnvVar != "SECRET_TOK" {
		t.Fatalf("get = %q %+v %v", v, m, err)
	}
	if _, err := s.Put("tok", "two", "CUSTOM"); err != nil {
		t.Fatal(err)
	}
	if v, m, _ := s.Get("tok"); v != "two" || m.EnvVar != "CUSTOM" {
		t.Fatalf("upsert = %q %+v", v, m)
	}
	s.Put("abc", "x", "")
	l := s.List()
	if len(l) != 2 || l[0].Name != "abc" || l[1].Name != "tok" {
		t.Fatalf("list = %+v", l)
	}
	if err := s.Delete("tok"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("tok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v", err)
	}
	if _, _, err := s.Get("tok"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete = %v", err)
	}
}

func TestPersistenceAndPerms(t *testing.T) {
	s, p := open(t)
	s.Put("tok", "secretvalue", "")
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v", fi.Mode(), err)
	}
	// A loosened file is re-tightened on open.
	os.Chmod(p, 0o644)
	s2, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after reopen = %v", fi.Mode())
	}
	if v, _, _ := s2.Get("tok"); v != "secretvalue" {
		t.Fatalf("reopen lost value")
	}
	s2.Put("tok", "again", "")
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode after rewrite = %v", fi.Mode())
	}
	// Corrupt store: error must not echo contents.
	os.WriteFile(p, []byte(`{"tok":{"value":"LEAKME"`), 0o600)
	if _, err := Open(p); err == nil || strings.Contains(err.Error(), "LEAKME") {
		t.Fatalf("corrupt open err = %v", err)
	}
}
