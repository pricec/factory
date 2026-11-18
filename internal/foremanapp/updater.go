package foremanapp

// Foreman self-updater (auto-update v1, spec §12; shared contract C3-C7).
//
// The factory advertises the newest foreman build in its capabilities
// response (C3). Each 60s caps push hands the advert to the Updater
// (Observe); when the advertised build is strictly Newer than the running
// one the Updater drains (C6: pauses NEW dispatch intake, never kills a
// worker or brain session), downloads + sha256-verifies the binary into
// <data>/bin/foreman.new, keeps the old binary as foreman.prev, writes the
// update-pending.json marker, rename(2)s the new binary over
// <data>/bin/foreman and exits 0 so systemd restarts it (C5). The new
// build confirms itself after its first successful caps push (C7) or is
// rolled back to foreman.prev after 3 minutes; a build that was rolled
// back is blocklisted until a newer one is advertised.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pricec/factory/internal/claudeauth"

	"github.com/pricec/factory/internal/buildinfo"
)

// Auto-update layout (C5): every file lives in <data>/bin.
const (
	updateBinDir      = "bin"
	updateLiveName    = "foreman"
	updatePrevName    = "foreman.prev"
	updateNewName     = "foreman.new"
	updateMarkerName  = "update-pending.json"
	updateBootsName   = "update-boots"
	updateBlockName   = "update-blocklist"
	updateTmpSuffix   = ".tmp"
	updateProbePrefix = ".update-probe-"
)

// Auto-update timing (C6, C7).
const (
	// DefaultDrainCeiling bounds how long new dispatch intake stays paused
	// waiting for live work to finish (C6).
	DefaultDrainCeiling = 10 * time.Minute
	// DefaultConfirmTimeout bounds how long a freshly swapped build has to
	// confirm itself before it rolls back to foreman.prev (C7).
	DefaultConfirmTimeout = 3 * time.Minute
	// DefaultUpdatePoll is how often the updater re-checks an in-progress
	// drain and the confirm deadline between caps pushes.
	DefaultUpdatePoll = 5 * time.Second
	// maxUpdateDownload caps a downloaded binary (a foreman is ~20 MiB).
	maxUpdateDownload = 512 << 20
	// DefaultDownloadTimeout bounds one download (a hung transfer must
	// not wedge the updater).
	DefaultDownloadTimeout = 5 * time.Minute
)

// UpdateAdvert is the factory's foreman_update advert (C3).
type UpdateAdvert struct {
	Build     string                    `json:"build"`
	Platforms map[string]UpdatePlatform `json:"platforms"`
}

// UpdatePlatform is one advertised platform binary (C3).
type UpdatePlatform struct {
	SHA256 string `json:"sha256"`
	Path   string `json:"path"`
}

// capsResponse is the POST /v1/foreman/capabilities 200 body (C3).
type capsResponse struct {
	ForemanUpdate *UpdateAdvert `json:"foreman_update"`
}

// parseCapsResponse extracts the advert from a caps push response: 204 (or
// any non-200) means nothing advertised; a malformed 200 body is an error
// (the caller logs it and treats it as nothing advertised).
func parseCapsResponse(status int, body []byte) (*UpdateAdvert, error) {
	if status != http.StatusOK || len(strings.TrimSpace(string(body))) == 0 {
		return nil, nil
	}
	var r capsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, fmt.Errorf("capabilities response: %w", err)
	}
	return r.ForemanUpdate, nil
}

// parseClaudeDirective extracts the claude_auth block of a caps push
// response (spec §13.3); nil when absent or unreadable.
func parseClaudeDirective(status int, body []byte) *claudeauth.Directive {
	if status != http.StatusOK || len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	var r struct {
		ClaudeAuth *claudeauth.Directive `json:"claude_auth"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil
	}
	return r.ClaudeAuth
}

// updateMarker is bin/update-pending.json (C5, C7, C8).
type updateMarker struct {
	From       string `json:"from"`
	To         string `json:"to"`
	StartedAt  string `json:"started_at"`
	Attempts   int    `json:"attempts"`
	RolledBack bool   `json:"rolled_back,omitempty"`
}

// Updater is the foreman self-updater. Every external effect goes through
// a field so the swap/rollback/drain paths are unit-testable; zero values
// take the production defaults (see defaults).
type Updater struct {
	// DataDir is the foreman data dir; the layout lives in DataDir/bin.
	DataDir string
	// Running is the running build id (buildinfo.Current()).
	Running string
	// Platform is "<GOOS>-<GOARCH>", the advert's platform key.
	Platform string
	// FactoryURL resolves the advert's relative download paths.
	FactoryURL string
	// Bearer is sent on the download (the endpoint is unauthenticated
	// today; harmless and future-proof).
	Bearer string
	// HTTPClient downloads; nil -> http.DefaultClient.
	HTTPClient *http.Client
	// Now is the clock; nil -> time.Now.
	Now func() time.Time
	// Executable resolves the running binary; nil -> os.Executable.
	Executable func() (string, error)
	// Exit ends the process with code (after a swap: 0; after a
	// rollback: 1). Required.
	Exit func(code int)
	// Busy reports live work (workers, brain session, in-flight
	// dispatches) and a human description. nil -> never busy.
	Busy func() (bool, string)
	// Pause stops NEW dispatch intake; Resume restores it. nil -> no-op.
	Pause  func()
	Resume func()
	// SelfCheck runs the confirm self-checks (stores open, listener up);
	// nil -> always OK.
	SelfCheck func(ctx context.Context) error
	// DrainCeiling, ConfirmTimeout, Poll, DownloadTimeout: zero -> the
	// defaults above.
	DrainCeiling    time.Duration
	ConfirmTimeout  time.Duration
	Poll            time.Duration
	DownloadTimeout time.Duration
	// Rename and OpenFile are the filesystem seams (failure injection);
	// nil -> os.Rename / os.OpenFile.
	Rename   func(oldpath, newpath string) error
	OpenFile func(name string, flag int, perm os.FileMode) (*os.File, error)
	// Logf logs; nil -> log.Printf.
	Logf func(format string, args ...any)

	once sync.Once
	kick chan struct{}

	advMu  sync.Mutex
	advert *UpdateAdvert

	// mu serializes Step/Boot/Confirm/Poll and guards the state below.
	mu           sync.Mutex
	booted       bool
	bootAt       time.Time
	pending      *updateMarker // unconfirmed swap (marker.to == Running)
	exiting      bool
	draining     bool
	drainBuild   string
	drainStarted time.Time
	drainSpent   map[string]bool // builds whose drain hit the ceiling
	warned       map[string]bool // log-once keys
	attempts     map[string]int
	blocked      []string
}

func (u *Updater) init() {
	u.once.Do(func() {
		u.kick = make(chan struct{}, 1)
		u.drainSpent = map[string]bool{}
		u.warned = map[string]bool{}
		u.attempts = map[string]int{}
	})
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) logf(format string, args ...any) {
	if u.Logf != nil {
		u.Logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

func (u *Updater) logOnce(key, format string, args ...any) {
	if u.warned[key] {
		return
	}
	u.warned[key] = true
	u.logf(format, args...)
}

func (u *Updater) rename(oldpath, newpath string) error {
	if u.Rename != nil {
		return u.Rename(oldpath, newpath)
	}
	return os.Rename(oldpath, newpath)
}

func (u *Updater) openFile(name string, flag int, perm os.FileMode) (*os.File, error) {
	if u.OpenFile != nil {
		return u.OpenFile(name, flag, perm)
	}
	return os.OpenFile(name, flag, perm)
}

func (u *Updater) drainCeiling() time.Duration {
	if u.DrainCeiling > 0 {
		return u.DrainCeiling
	}
	return DefaultDrainCeiling
}

func (u *Updater) confirmTimeout() time.Duration {
	if u.ConfirmTimeout > 0 {
		return u.ConfirmTimeout
	}
	return DefaultConfirmTimeout
}

func (u *Updater) poll() time.Duration {
	if u.Poll > 0 {
		return u.Poll
	}
	return DefaultUpdatePoll
}

func (u *Updater) binDir() string {
	dir := u.DataDir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return filepath.Join(dir, updateBinDir)
}

func (u *Updater) binPath(name string) string { return filepath.Join(u.binDir(), name) }

// Observe records the advert from one caps push (nil: nothing advertised,
// e.g. 204) and schedules a Step.
func (u *Updater) Observe(adv *UpdateAdvert) {
	u.init()
	u.advMu.Lock()
	u.advert = adv
	u.advMu.Unlock()
	select {
	case u.kick <- struct{}{}:
	default:
	}
}

func (u *Updater) currentAdvert() *UpdateAdvert {
	u.advMu.Lock()
	defer u.advMu.Unlock()
	return u.advert
}

// Run drives the updater until ctx ends: a Step after every Observe, and a
// Poll every u.Poll (drain progress, confirm deadline).
func (u *Updater) Run(ctx context.Context) {
	u.init()
	t := time.NewTicker(u.poll())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-u.kick:
			u.Step(ctx)
		case <-t.C:
			u.PollOnce(ctx)
		}
	}
}

// Boot handles a leftover marker at startup (C7) and loads the blocklist.
// It must run once, before the first caps push.
func (u *Updater) Boot() {
	u.init()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.booted {
		return
	}
	u.booted = true
	u.bootAt = u.now()
	u.loadBlocklist()

	data, err := os.ReadFile(u.binPath(updateMarkerName))
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err != nil {
		u.logf("auto-update: read marker: %v (ignored)", err)
		return
	}
	var m updateMarker
	if err := json.Unmarshal(data, &m); err != nil || m.To == "" {
		u.logf("auto-update: AUTO-UPDATE MARKER CORRUPT (%v): %q; discarding", err, data)
		u.clearMarker()
		return
	}
	switch {
	case m.To == u.Running:
		u.pending = &m
		u.logf("auto-update: booted build %s (from %s, attempt %d); confirming within %s",
			m.To, m.From, m.Attempts, u.confirmTimeout())
	case m.RolledBack:
		u.logf("auto-update: AUTO-UPDATE ROLLED BACK: build %s failed to confirm; running %s again; "+
			"refusing build %s until a newer build is advertised", m.To, u.Running, m.To)
		u.addBlock(m.To)
		u.clearMarker()
	default:
		// The swap never happened (the process stopped between writing
		// the marker and renaming the new binary), or an operator
		// installed a different build by hand: not a failed boot of m.To.
		u.logf("auto-update: AUTO-UPDATE INTERRUPTED: marker %s -> %s found while running %s "+
			"(no rollback recorded); discarding marker, build %s stays eligible", m.From, m.To, u.Running, m.To)
		u.clearMarker()
	}
}

// Pending reports whether this process is an unconfirmed swapped build.
func (u *Updater) Pending() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.pending != nil
}

// Blocked reports whether build is blocklisted (C7): refused until a build
// strictly newer than every blocklisted one is advertised.
func (u *Updater) Blocked(build string) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.isBlocked(build)
}

func (u *Updater) isBlocked(build string) bool {
	for _, b := range u.blocked {
		if b == build || !buildinfo.Newer(build, b) && validBuild(b) {
			return true
		}
	}
	return false
}

func validBuild(id string) bool {
	_, ok := buildinfo.Time(id)
	return ok
}

func (u *Updater) loadBlocklist() {
	data, err := os.ReadFile(u.binPath(updateBlockName))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			u.logf("auto-update: read blocklist: %v", err)
		}
		return
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			u.blocked = append(u.blocked, line)
		}
	}
}

func (u *Updater) addBlock(build string) {
	for _, b := range u.blocked {
		if b == build {
			return
		}
	}
	u.blocked = append(u.blocked, build)
	data := []byte(strings.Join(u.blocked, "\n") + "\n")
	if err := u.writeFileAtomic(u.binPath(updateBlockName), data, 0o644); err != nil {
		u.logf("auto-update: write blocklist: %v (build %s refused for this process only)", err, build)
	}
}

// clearMarker removes the marker and the guard's boot counter (C7, C8).
func (u *Updater) clearMarker() {
	for _, name := range []string{updateMarkerName, updateBootsName} {
		if err := os.Remove(u.binPath(name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			u.logf("auto-update: remove %s: %v", name, err)
		}
	}
}

// Confirm runs after every successful caps push (C7): a pending swapped
// build whose self-checks pass is confirmed (marker + boot counter
// deleted). A failed self-check is retried at the next push.
func (u *Updater) Confirm(ctx context.Context) {
	u.init()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.pending == nil || u.exiting {
		return
	}
	if u.SelfCheck != nil {
		if err := u.SelfCheck(ctx); err != nil {
			u.logf("auto-update: build %s not confirmed yet: self-check: %v", u.Running, err)
			return
		}
	}
	u.clearMarker()
	u.logf("auto-update: build %s confirmed (updated from %s after %s)",
		u.Running, u.pending.From, u.now().Sub(u.bootAt).Round(time.Second))
	u.pending = nil
}

// PollOnce enforces the confirm deadline and advances an in-progress drain.
func (u *Updater) PollOnce(ctx context.Context) {
	u.init()
	u.mu.Lock()
	if u.pending != nil && !u.exiting && u.now().Sub(u.bootAt) >= u.confirmTimeout() {
		u.rollbackLocked()
		u.mu.Unlock()
		return
	}
	draining := u.draining
	u.mu.Unlock()
	if draining {
		u.Step(ctx)
	}
}

// rollbackLocked restores foreman.prev over foreman, marks the marker
// rolled_back and exits non-zero (C7). Without a usable foreman.prev there
// is nothing to roll back to: log loudly and keep running the new build.
func (u *Updater) rollbackLocked() {
	m := *u.pending
	u.logf("auto-update: AUTO-UPDATE CONFIRM TIMEOUT: build %s not confirmed within %s; rolling back to %s",
		u.Running, u.confirmTimeout(), m.From)
	if err := u.checkLayout(); err != nil {
		u.logf("auto-update: AUTO-UPDATE ROLLBACK IMPOSSIBLE: %v; keeping build %s", err, u.Running)
		u.clearMarker()
		u.pending = nil
		return
	}
	prev := u.binPath(updatePrevName)
	if _, err := os.Stat(prev); err != nil {
		u.logf("auto-update: AUTO-UPDATE ROLLBACK IMPOSSIBLE: %v; keeping build %s", err, u.Running)
		u.clearMarker()
		u.pending = nil
		return
	}
	live := u.binPath(updateLiveName)
	tmp := live + ".rollback" + updateTmpSuffix
	if err := u.copyFile(prev, tmp); err != nil {
		os.Remove(tmp)
		u.logf("auto-update: AUTO-UPDATE ROLLBACK FAILED: copy %s: %v; retrying", prev, err)
		return
	}
	if err := u.rename(tmp, live); err != nil {
		os.Remove(tmp)
		u.logf("auto-update: AUTO-UPDATE ROLLBACK FAILED: %v; retrying", err)
		return
	}
	syncDir(u.binDir())
	m.RolledBack = true
	if err := u.writeMarker(m); err != nil {
		// The old binary is back in place; without the flag it would
		// treat the marker as an interrupted swap and stay eligible -
		// still safe (C8 guard and this timeout bound any loop).
		u.logf("auto-update: rollback marker: %v", err)
	}
	u.pending = nil
	u.exiting = true
	u.logf("auto-update: rolled back to %s; exiting 1 for restart", m.From)
	u.Exit(1)
}

// Step evaluates the current advert (C6): update now when idle, otherwise
// drain (pause new intake) for up to the ceiling. Failures keep serving
// and retry at the next caps push.
func (u *Updater) Step(ctx context.Context) {
	u.init()
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.exiting || u.pending != nil {
		return // never chain updates before the running build is confirmed
	}
	adv := u.currentAdvert()
	if adv == nil || !buildinfo.Newer(adv.Build, u.Running) {
		u.endDrain("no newer build advertised")
		return
	}
	build := adv.Build
	if u.isBlocked(build) {
		u.logOnce("blocked:"+build, "auto-update: build %s advertised but blocklisted after a rollback; waiting for a newer build", build)
		u.endDrain("advertised build blocklisted")
		return
	}
	plat, ok := adv.Platforms[u.Platform]
	if !ok || plat.SHA256 == "" || plat.Path == "" {
		u.logOnce("platform:"+build, "auto-update: build %s advertised without a %s binary; not updating", build, u.Platform)
		u.endDrain("platform not advertised")
		return
	}
	if err := u.checkLayout(); err != nil {
		u.logOnce("layout:"+build, "auto-update: AUTO-UPDATE DISABLED: binary not in writable layout (%v); build %s advertised, running %s",
			err, build, u.Running)
		u.endDrain("not in writable layout")
		return
	}

	// Pause BEFORE sampling liveness: once paused no new dispatch can
	// begin, so an idle sample stays idle through the swap.
	wasDraining := u.draining
	if !wasDraining {
		if u.Pause != nil {
			u.Pause()
		}
	}
	busy, detail := false, ""
	if u.Busy != nil {
		busy, detail = u.Busy()
	}
	if !busy {
		if err := u.apply(ctx, build, plat); err != nil {
			u.logf("auto-update: AUTO-UPDATE FAILED for build %s: %v; still serving %s, retrying next tick", build, err, u.Running)
			if wasDraining {
				u.endDrain("update failed")
			} else if u.Resume != nil {
				u.Resume()
			}
			return
		}
		return // exiting; intake stays paused
	}
	if !wasDraining {
		if u.drainSpent[build] {
			// The ceiling already passed for this build: no new drain,
			// only an idle tick updates (C6).
			if u.Resume != nil {
				u.Resume()
			}
			return
		}
		u.draining = true
		u.drainBuild = build
		u.drainStarted = u.now()
		u.logf("auto-update: build %s advertised (running %s); draining: new dispatches paused for up to %s while busy (%s)",
			build, u.Running, u.drainCeiling(), detail)
		return
	}
	u.drainBuild = build
	if waited := u.now().Sub(u.drainStarted); waited >= u.drainCeiling() {
		u.drainSpent[build] = true
		u.endDrain(fmt.Sprintf("still busy after %s (%s); resuming intake, will update at the next idle tick", waited.Round(time.Second), detail))
	}
}

// endDrain resumes intake if a drain is in progress.
func (u *Updater) endDrain(reason string) {
	if !u.draining {
		return
	}
	u.draining = false
	if u.Resume != nil {
		u.Resume()
	}
	u.logf("auto-update: drain for build %s ended: %s", u.drainBuild, reason)
	u.drainBuild = ""
}

// Draining reports whether new intake is paused for an update.
func (u *Updater) Draining() bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.draining
}

// checkLayout verifies the C5 layout: the running executable IS
// <data>/bin/foreman and that directory is writable.
func (u *Updater) checkLayout() error {
	exe := os.Executable
	if u.Executable != nil {
		exe = u.Executable
	}
	path, err := exe()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	if r, err := filepath.EvalSymlinks(path); err == nil {
		path = r
	}
	want := u.binPath(updateLiveName)
	if r, err := filepath.EvalSymlinks(want); err == nil {
		want = r
	}
	if filepath.Clean(path) != filepath.Clean(want) {
		return fmt.Errorf("running %s, not %s", path, want)
	}
	probe := u.binPath(updateProbePrefix + strconv.Itoa(os.Getpid()))
	f, err := u.openFile(probe, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("%s not writable: %w", u.binDir(), err)
	}
	f.Close()
	os.Remove(probe)
	return nil
}

// apply stages, verifies and swaps in build, then exits 0 (C5). It is
// all-or-nothing: any failure before the final rename leaves the live
// binary, foreman.prev and the marker exactly as they were.
func (u *Updater) apply(ctx context.Context, build string, plat UpdatePlatform) (err error) {
	u.attempts[build]++
	live := u.binPath(updateLiveName)
	staged := u.binPath(updateNewName)
	prevTmp := u.binPath(updatePrevName + updateTmpSuffix)
	markerWritten := false
	defer func() {
		if err == nil {
			return
		}
		os.Remove(staged)
		os.Remove(prevTmp)
		if markerWritten {
			os.Remove(u.binPath(updateMarkerName))
		}
	}()

	u.logf("auto-update: downloading build %s for %s", build, u.Platform)
	if err := u.download(ctx, plat, staged); err != nil {
		return err
	}
	if err := u.copyFile(live, prevTmp); err != nil {
		return fmt.Errorf("save %s: %w", updatePrevName, err)
	}
	if err := u.rename(prevTmp, u.binPath(updatePrevName)); err != nil {
		return fmt.Errorf("save %s: %w", updatePrevName, err)
	}
	// A stale guard counter must not count against this attempt.
	if err := os.Remove(u.binPath(updateBootsName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("reset %s: %w", updateBootsName, err)
	}
	m := updateMarker{
		From:      u.Running,
		To:        build,
		StartedAt: u.now().UTC().Format(time.RFC3339),
		Attempts:  u.attempts[build],
	}
	markerWritten = true
	if err := u.writeMarker(m); err != nil {
		return fmt.Errorf("write marker: %w", err)
	}
	if err := u.rename(staged, live); err != nil {
		return fmt.Errorf("swap: %w", err)
	}
	syncDir(u.binDir())
	u.exiting = true
	u.logf("auto-update: swapped in build %s (from %s, attempt %d); exiting 0 for restart", build, u.Running, m.Attempts)
	u.Exit(0)
	return nil
}

// download fetches the advertised binary into dst (fsynced, 0755) and
// verifies its size and sha256.
func (u *Updater) download(ctx context.Context, plat UpdatePlatform, dst string) error {
	target := plat.Path
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		if !strings.HasPrefix(target, "/") {
			return fmt.Errorf("download path %q: not absolute", target)
		}
		target = strings.TrimRight(u.FactoryURL, "/") + target
	}
	timeout := u.DownloadTimeout
	if timeout <= 0 {
		timeout = DefaultDownloadTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if u.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+u.Bearer)
	}
	client := u.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: status %d", target, resp.StatusCode)
	}
	f, err := u.openFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("stage: %w", err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxUpdateDownload+1))
	if err == nil && n > maxUpdateDownload {
		err = fmt.Errorf("larger than %d bytes", maxUpdateDownload)
	}
	if err == nil && resp.ContentLength >= 0 && n != resp.ContentLength {
		err = fmt.Errorf("short download: %d of %d bytes", n, resp.ContentLength)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", target, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, plat.SHA256) {
		return fmt.Errorf("download %s: sha256 mismatch: got %s, advertised %s", target, got, plat.SHA256)
	}
	if err := os.Chmod(dst, 0o755); err != nil {
		return fmt.Errorf("stage chmod: %w", err)
	}
	return nil
}

// copyFile copies src to dst (fsynced, 0755).
func (u *Updater) copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := u.openFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

func (u *Updater) writeMarker(m updateMarker) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return u.writeFileAtomic(u.binPath(updateMarkerName), data, 0o644)
}

// writeFileAtomic writes data to path via a fsynced temp file + rename.
func (u *Updater) writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + updateTmpSuffix
	f, err := u.openFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = u.rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	syncDir(filepath.Dir(path))
	return nil
}

// syncDir fsyncs a directory so renames inside it are durable (best
// effort: not every filesystem supports it).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
