package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// ForemanUpdate is the foreman_update advert (spec §6.7 refresh response).
type ForemanUpdate struct {
	Build     string                    `json:"build"`
	Platforms map[string]PlatformAdvert `json:"platforms"`
}

// PlatformAdvert is one platform's downloadable foreman binary.
type PlatformAdvert struct {
	SHA256 string `json:"sha256"`
	Path   string `json:"path"`
}

// AdvertFunc returns the current advert, or nil when nothing is
// advertisable (the refresh then answers 204 exactly as before).
type AdvertFunc func() *ForemanUpdate

type manifestFile struct {
	Build string `json:"build"`
	Files map[string]struct {
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

var (
	hexSHA      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	foremanFile = regexp.MustCompile(`^foreman-(linux-(?:amd64|arm64))$`)
)

type hashEntry struct {
	size  int64
	mtime time.Time
	sum   string
}

// Advertiser derives the foreman_update advert from <dir>/manifest.json,
// re-hashing each advertised binary itself (cached by path+size+mtime) so
// a stale or half-copied deploy is never advertised.
type Advertiser struct {
	dir  string
	logf func(format string, args ...any)

	mu      sync.Mutex
	hashes  map[string]hashEntry
	lastMsg string // last problem logged; suppresses repeats
}

// NewAdvertiser serves adverts from the downloads dir.
func NewAdvertiser(dir string) *Advertiser {
	return &Advertiser{dir: dir, logf: log.Printf, hashes: map[string]hashEntry{}}
}

// Advert implements AdvertFunc.
func (a *Advertiser) Advert() *ForemanUpdate {
	a.mu.Lock()
	defer a.mu.Unlock()

	raw, err := os.ReadFile(filepath.Join(a.dir, "manifest.json"))
	if err != nil {
		if os.IsNotExist(err) {
			a.problem("") // absent manifest is the normal pre-feature state
		} else {
			a.problem("read manifest: " + err.Error())
		}
		return nil
	}
	var m manifestFile
	if err := json.Unmarshal(raw, &m); err != nil {
		a.problem("manifest malformed: " + err.Error())
		return nil
	}
	if m.Build == "" {
		a.problem("manifest has empty build")
		return nil
	}

	var msgs []string
	out := &ForemanUpdate{Build: m.Build, Platforms: map[string]PlatformAdvert{}}
	names := make([]string, 0, len(m.Files))
	for n := range m.Files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		sub := foremanFile.FindStringSubmatch(name)
		if sub == nil {
			continue
		}
		want := strings.ToLower(m.Files[name].SHA256)
		if !hexSHA.MatchString(want) {
			msgs = append(msgs, name+": invalid sha256 in manifest")
			continue
		}
		got, err := a.hash(filepath.Join(a.dir, name))
		if err != nil {
			msgs = append(msgs, name+": "+err.Error())
			continue
		}
		if got != want {
			msgs = append(msgs, name+": sha256 mismatch (manifest "+want+", file "+got+")")
			continue
		}
		out.Platforms[sub[1]] = PlatformAdvert{SHA256: got, Path: "/download/foreman/" + name}
	}
	if len(msgs) > 0 {
		a.problem("build " + m.Build + ": omitting platforms: " + strings.Join(msgs, "; "))
	} else {
		a.problem("")
	}
	if len(out.Platforms) == 0 {
		return nil
	}
	return out
}

// problem logs msg loudly once per distinct message; "" clears state.
func (a *Advertiser) problem(msg string) {
	if msg == a.lastMsg {
		return
	}
	a.lastMsg = msg
	if msg != "" {
		a.logf("registry: AUTO-UPDATE ADVERT: %s", msg)
	}
}

func (a *Advertiser) hash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if c, ok := a.hashes[path]; ok && c.size == st.Size() && c.mtime.Equal(st.ModTime()) {
		return c.sum, nil
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	sum := hex.EncodeToString(h.Sum(nil))
	a.hashes[path] = hashEntry{size: st.Size(), mtime: st.ModTime(), sum: sum}
	return sum, nil
}
