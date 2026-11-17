// Package secrets is the factory's credentials store (spec §13): named
// values with an env-var mapping, kept in one plaintext JSON file with
// mode 0600 in the factory data dir. There is no encryption at rest; the
// trust boundary is the host. Values are never logged and never listed:
// List returns metadata only, and every error carries names only.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileName is the store file inside the factory data dir.
const FileName = "secrets.json"

// MaxValueBytes caps a secret value.
const MaxValueBytes = 64 << 10

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	envVarRE = regexp.MustCompile(`^[A-Z_0-9]+$`)

	// ErrNotFound: no secret with that name.
	ErrNotFound = errors.New("secret not found")
	// ErrInvalid wraps every validation failure.
	ErrInvalid = errors.New("invalid secret")
)

// Meta is the listable view of a secret: never the value.
type Meta struct {
	Name      string `json:"name"`
	EnvVar    string `json:"env_var"`
	UpdatedAt string `json:"updated_at"`
}

type entry struct {
	Value     string `json:"value"`
	EnvVar    string `json:"env_var"`
	UpdatedAt string `json:"updated_at"`
}

// Store is a file-backed secrets store, safe for concurrent use.
type Store struct {
	Now func() time.Time

	mu   sync.Mutex
	path string
	m    map[string]entry
}

// Open loads (or creates) the store at path, forcing mode 0600.
func Open(path string) (*Store, error) {
	s := &Store{Now: func() time.Time { return time.Now().UTC() }, path: path, m: map[string]entry{}}
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) > 0 {
			if err := json.Unmarshal(data, &s.m); err != nil {
				// Never include data: it contains values.
				return nil, fmt.Errorf("secrets: parse %s: malformed store", filepath.Base(path))
			}
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("secrets: chmod store: %w", err)
		}
	case errors.Is(err, os.ErrNotExist):
		if err := s.persist(); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("secrets: read store: %w", err)
	}
	return s, nil
}

// DefaultEnvVar derives SECRET_<UPPER_SNAKE(name)>.
func DefaultEnvVar(name string) string {
	return "SECRET_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// ValidateName checks the name rule.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("%w: name %q must match [a-z0-9][a-z0-9-]{0,63}", ErrInvalid, name)
	}
	return nil
}

// Put upserts name. An empty envVar means the derived default.
func (s *Store) Put(name, value, envVar string) (Meta, error) {
	if err := ValidateName(name); err != nil {
		return Meta{}, err
	}
	if value == "" {
		return Meta{}, fmt.Errorf("%w: value for %s must be non-empty", ErrInvalid, name)
	}
	if len(value) > MaxValueBytes {
		return Meta{}, fmt.Errorf("%w: value for %s exceeds %d bytes", ErrInvalid, name, MaxValueBytes)
	}
	if envVar == "" {
		envVar = DefaultEnvVar(name)
	} else if !envVarRE.MatchString(envVar) {
		return Meta{}, fmt.Errorf("%w: env_var for %s must match [A-Z_0-9]+", ErrInvalid, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had := s.m[name]
	e := entry{Value: value, EnvVar: envVar, UpdatedAt: s.Now().UTC().Format(time.RFC3339)}
	s.m[name] = e
	if err := s.persist(); err != nil {
		if had {
			s.m[name] = prev
		} else {
			delete(s.m, name)
		}
		return Meta{}, fmt.Errorf("secrets: store %s: %w", name, err)
	}
	return Meta{Name: name, EnvVar: e.EnvVar, UpdatedAt: e.UpdatedAt}, nil
}

// Missing returns the names (sorted, de-duplicated) that are not in the
// store. Names only; used for creation-time validation (spec §13.2).
func (s *Store) Missing(names []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if _, ok := s.m[n]; !ok && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// CheckNames validates a job's secrets list at creation: every name must
// exist. The error names the unknown secrets (never any value).
func (s *Store) CheckNames(names []string) error {
	if missing := s.Missing(names); len(missing) > 0 {
		return fmt.Errorf("unknown_secrets: %s not in the credentials store (GET /v1/secrets lists names)", strings.Join(missing, ", "))
	}
	return nil
}

// Get returns the value and metadata. Used only by the job handout
// endpoint (spec §13.2); no other route calls it.
func (s *Store) Get(name string) (string, Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[name]
	if !ok {
		return "", Meta{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return e.Value, Meta{Name: name, EnvVar: e.EnvVar, UpdatedAt: e.UpdatedAt}, nil
}

// Delete removes name; unknown names are ErrNotFound.
func (s *Store) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.m[name]
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	delete(s.m, name)
	if err := s.persist(); err != nil {
		s.m[name] = prev
		return fmt.Errorf("secrets: delete %s: %w", name, err)
	}
	return nil
}

// List returns metadata sorted by name. Never values.
func (s *Store) List() []Meta {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Meta, 0, len(s.m))
	for n, e := range s.m {
		out = append(out, Meta{Name: n, EnvVar: e.EnvVar, UpdatedAt: e.UpdatedAt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// persist writes the map atomically (0600 temp + rename). Caller holds mu
// (or is Open).
func (s *Store) persist() error {
	data, err := json.Marshal(s.m)
	if err != nil {
		return errors.New("encode store")
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".secrets-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
