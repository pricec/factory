// Claude CLI auth health and the operator re-login flow (spec §13.3).
//
// Detection: a job.result whose error reads as a Claude CLI auth failure
// (`Not logged in · Please run /login`, a 401 on the OAuth token, ...)
// marks that foreman's Claude auth failed, parks the job `held` (no retry
// burned, like the usage-limit pause) and asks the foreman to probe; the
// foreman's own probe verdict (pushed with its capabilities) is the
// authority from then on. While a foreman is failed, claude jobs are not
// admitted to it, not matched to it and not released to it; once its
// probe passes, held jobs release automatically.
//
// Operator flow: one task per auth episode (shared by every affected
// foreman, since one token fixes all of them) carries the login URL of a
// headless `claude setup-token` session; the operator pastes the code (or
// a token minted elsewhere) back into the thread. The factory consumes
// that reply WITHOUT storing it, verifies the token, stores it in the
// credentials store as `claude-oauth-token` (env CLAUDE_CODE_OAUTH_TOKEN)
// and every foreman adopts it on its next capabilities push, probes, and
// reports; the outcome is posted to the thread.
package factoryapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/claudeauth"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/wire"
)

const (
	// claudeAuthSettingKey persists the health book (tasks settings).
	claudeAuthSettingKey = "claude_auth_state"
	// claudeHoldPrefix marks a held_reason as a Claude-auth hold.
	claudeHoldPrefix = "claude auth: "
	// claudeAuthTaskPriority: blocked work, operator action required.
	claudeAuthTaskPriority = 1
	// claudeCodeTimeout bounds the code -> token exchange.
	claudeCodeTimeout = 90 * time.Second
	// claudeSetupStartTimeout bounds starting a setup session.
	claudeSetupStartTimeout = 60 * time.Second
	claudeAuthSource        = "factory:claude-auth"
)

// codeRe is what an OAuth code pasted back from the browser looks like:
// one word of URL-safe characters (the callback page shows `code#state`).
var (
	codeRe    = regexp.MustCompile(`^[A-Za-z0-9_\-#.~]{16,1000}$`)
	newLinkRe = regexp.MustCompile(`(?i)^\s*(new (login )?(link|url)|retry|restart|login)\s*[.!]?\s*$`)
)

// foremanClaude is one foreman's Claude auth health.
type foremanClaude struct {
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	// Since is when State was entered.
	Since time.Time `json:"since"`
	// FailedAt is the last result-detected failure (a probe report
	// checked before it is stale and cannot clear it).
	FailedAt time.Time `json:"failed_at,omitzero"`
	// CheckedAt is the foreman's last probe time.
	CheckedAt string `json:"checked_at,omitempty"`
	// Reported is true once the foreman pushes claude_auth reports;
	// Tiers is meaningful only then.
	Reported bool     `json:"reported,omitempty"`
	Tiers    []string `json:"claude_tiers,omitempty"`
	TokenRev string   `json:"token_rev,omitempty"`
	// ProbeWanted is delivered on the next caps response, then cleared.
	ProbeWanted bool `json:"probe_wanted,omitempty"`
}

// claudeBook is the persisted state.
type claudeBook struct {
	Foremen map[string]*foremanClaude `json:"foremen"`
	// TaskID is the open re-login task ("" = none).
	TaskID string `json:"task_id,omitempty"`
	// Announced lists the foremen already named in TaskID's thread.
	Announced []string `json:"announced,omitempty"`
}

// setupHandle is a running `claude setup-token` session (test seam over
// *claudeauth.SetupSession).
type setupHandle interface {
	Submit(ctx context.Context, code string, timeout time.Duration) (string, error)
	Expired(now time.Time) bool
	Close()
}

// claudeAuthRuntime is the App's Claude-auth state.
type claudeAuthRuntime struct {
	mu     sync.Mutex
	book   *claudeBook
	setup  setupHandle
	busy   bool // a code/token is being processed
	prober claudeauth.Prober
	// startSetup is the test seam for claudeauth.StartSetup.
	startSetup func(ctx context.Context) (setupHandle, string, error)
	// async runs background work (tests make it synchronous).
	async func(func())
}

func newClaudeAuthRuntime() *claudeAuthRuntime {
	return &claudeAuthRuntime{
		startSetup: func(ctx context.Context) (setupHandle, string, error) {
			s, url, err := claudeauth.StartSetup(ctx, claudeauth.SetupConfig{})
			if err != nil {
				return nil, "", err
			}
			return s, url, nil
		},
		async: func(f func()) { go f() },
	}
}

// foremanKey normalizes a foreman id to the "foreman:{id}" identity.
func foremanKey(id string) string {
	if id == "" || strings.HasPrefix(id, "foreman:") {
		return id
	}
	return "foreman:" + id
}

// ---- persistence ----

func (a *App) claudeBookLocked(ctx context.Context) *claudeBook {
	c := a.claude
	if c.book != nil {
		return c.book
	}
	c.book = &claudeBook{Foremen: map[string]*foremanClaude{}}
	if a.tasks == nil {
		return c.book
	}
	raw, ok, err := a.tasks.GetSetting(ctx, claudeAuthSettingKey)
	if err != nil {
		log.Printf("factoryapp: claude auth: load state: %v", err)
		return c.book
	}
	if ok && raw != "" {
		var b claudeBook
		if err := json.Unmarshal([]byte(raw), &b); err != nil {
			log.Printf("factoryapp: claude auth: ignoring unreadable state: %v", err)
		} else {
			if b.Foremen == nil {
				b.Foremen = map[string]*foremanClaude{}
			}
			c.book = &b
		}
	}
	return c.book
}

func (a *App) saveClaudeBookLocked(ctx context.Context) {
	if a.tasks == nil || a.claude.book == nil {
		return
	}
	data, err := json.Marshal(a.claude.book)
	if err != nil {
		return
	}
	if err := a.tasks.SetSetting(ctx, claudeAuthSettingKey, string(data)); err != nil {
		log.Printf("factoryapp: claude auth: save state: %v", err)
	}
}

func (b *claudeBook) entry(id string) *foremanClaude {
	e := b.Foremen[id]
	if e == nil {
		e = &foremanClaude{State: claudeauth.StateUnknown}
		b.Foremen[id] = e
	}
	return e
}

func (b *claudeBook) failed() []string {
	var out []string
	for id, e := range b.Foremen {
		if e.State == claudeauth.StateFailed {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// ---- token ----

// claudeToken returns the stored OAuth token and its revision ("" when
// none). The revision is a short hash of value and update time: it lets
// foremen notice a change without the value ever riding the caps push.
func (a *App) claudeToken() (value, rev string) {
	if a.secrets == nil {
		return "", ""
	}
	v, meta, err := a.secrets.Get(claudeauth.SecretName)
	if err != nil || v == "" {
		return "", ""
	}
	sum := sha256.Sum256([]byte(meta.UpdatedAt + "\x00" + v))
	return v, hex.EncodeToString(sum[:6])
}

// claudeEnv is the planner's claude-session env (spec §13.3).
func (a *App) claudeEnv() map[string]string {
	if v, _ := a.claudeToken(); v != "" {
		return map[string]string{claudeauth.TokenEnv: v}
	}
	return nil
}

// handleClaudeToken serves GET /v1/foreman/claude-token (paired foreman
// bearer): {value, rev}, 404 when no token is stored. The value is never
// logged; errors carry no value.
func (a *App) handleClaudeToken(w http.ResponseWriter, r *http.Request) {
	var identity string
	if err := a.refreshAuthz()(r, &identity); err != nil {
		status := http.StatusUnauthorized
		var fe *auth.ForbiddenError
		if errors.As(err, &fe) {
			status = http.StatusForbidden
		}
		writeError(w, status, "unauthorized", err.Error())
		return
	}
	v, rev := a.claudeToken()
	if v == "" {
		writeError(w, http.StatusNotFound, "unknown_secret", "no "+claudeauth.SecretName+" stored")
		return
	}
	log.Printf("factoryapp: claude auth: token rev %s handed to %s", rev, identity)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"value": v, "rev": rev})
}

// ---- foreman reports (caps push extension) ----

// claudeCapsExt is the registry.ExtensionFunc: it folds the foreman's
// claude_auth report into the book and answers with the directive.
func (a *App) claudeCapsExt(ctx context.Context, identity string, raw json.RawMessage) map[string]any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil // an older foreman: nothing reported, nothing to direct
	}
	var rep claudeauth.Report
	if err := json.Unmarshal(raw, &rep); err != nil {
		log.Printf("factoryapp: claude auth: bad report from %s: %v", identity, err)
		return nil
	}
	id := foremanKey(identity)
	_, rev := a.claudeToken()
	var toOK, toFailed bool
	var detail string
	a.claude.mu.Lock()
	b := a.claudeBookLocked(ctx)
	e := b.entry(id)
	e.Reported, e.Tiers, e.TokenRev, e.CheckedAt = true, rep.ClaudeTiers, rep.TokenRev, rep.CheckedAt
	if e.Tiers == nil {
		e.Tiers = []string{}
	}
	checked, _ := time.Parse(time.RFC3339, rep.CheckedAt)
	switch rep.State {
	case claudeauth.StateOK:
		// A probe older than the last result-detected failure is stale.
		if e.State != claudeauth.StateOK && (e.FailedAt.IsZero() || !checked.Before(e.FailedAt.Truncate(time.Second))) {
			toOK = e.State == claudeauth.StateFailed
			e.State, e.Since, e.Detail, e.FailedAt = claudeauth.StateOK, a.clock(), rep.Detail, time.Time{}
		}
	case claudeauth.StateFailed:
		if e.State != claudeauth.StateFailed {
			toFailed = true
			e.State, e.Since = claudeauth.StateFailed, a.clock()
		}
		e.Detail = claudeauth.Redact(rep.Detail)
		detail = e.Detail
	}
	// Keep asking for a probe until a verdict newer than the last
	// result-detected failure arrives (the foreman debounces probes).
	stale := e.State == claudeauth.StateFailed && !e.FailedAt.IsZero() && checked.Before(e.FailedAt.Truncate(time.Second))
	dir := claudeauth.Directive{TokenRev: rev, Probe: e.ProbeWanted || stale}
	e.ProbeWanted = false
	a.saveClaudeBookLocked(ctx)
	a.claude.mu.Unlock()

	if toFailed {
		log.Printf("factoryapp: claude auth: %s reports the Claude CLI is not authenticated: %s", id, detail)
		a.claudeEpisode(ctx, id, detail)
	}
	if toOK {
		a.claudeRestored(ctx, id)
	}
	return map[string]any{"claude_auth": dir}
}

// ---- detection from job results ----

// requeueClaudeAuth is the job.result hook: a failed claude run whose
// error is an auth failure marks the foreman failed and parks the job held
// (no failed state, no retry burned). handled reports the result was
// consumed.
func (a *App) requeueClaudeAuth(ctx context.Context, res wire.JobResult) (handled bool) {
	if res.Status != wire.StatusFailed || res.Error == nil || res.JobID == "" {
		return false
	}
	if !claudeauth.IsAuthFailure(res.Error.Message) && !claudeauth.IsAuthFailure(res.Summary) {
		return false
	}
	job, ok, err := a.jobs.Get(ctx, res.JobID)
	if err != nil || !ok || job.ForemanID == "" {
		return false
	}
	id := foremanKey(job.ForemanID)
	a.claude.mu.Lock()
	e := a.claudeBookLocked(ctx).entry(id)
	var tiers []string
	if e.Reported {
		tiers = e.Tiers
	}
	a.claude.mu.Unlock()
	if !claudeauth.RunsClaude(job.Spec.Model, job.Spec.Tier, tiers) {
		return false // not a claude run here: some other tool's login
	}
	detail := claudeauth.Redact(strings.TrimSpace(res.Error.Message))
	if r := []rune(detail); len(r) > 200 {
		detail = string(r[:200])
	}
	a.markClaudeFailed(ctx, id, detail)
	if a.isMergeJob(ctx, job.ID) {
		return false // merge jobs never re-run outside the queue (§6.11)
	}
	reason := claudeHoldReason(id)
	applied, err := a.jobs.RequeueHeld(ctx, res.JobID, reason)
	if err != nil || !applied {
		if err != nil {
			log.Printf("factoryapp: claude auth: requeue %s: %v", res.JobID, err)
		}
		return false
	}
	log.Printf("factoryapp: job %s failed on Claude auth at %s: requeued held; no retry consumed", res.JobID, id)
	if taskID, ok, err := a.tasks.TaskForJob(ctx, res.JobID); err == nil && ok {
		a.publishTaskChanged(ctx, taskID)
	}
	return true
}

func claudeHoldReason(foremanID string) string {
	return claudeHoldPrefix + foremanID + " is not logged in to the Claude CLI; held until the operator restores it (see the Claude login task)"
}

// markClaudeFailed records a result-detected failure: state failed, a
// probe requested (the foreman's verdict confirms or clears it), and the
// operator task opened.
func (a *App) markClaudeFailed(ctx context.Context, id, detail string) {
	a.claude.mu.Lock()
	e := a.claudeBookLocked(ctx).entry(id)
	was := e.State
	now := a.clock()
	e.State, e.Detail, e.FailedAt, e.ProbeWanted = claudeauth.StateFailed, detail, now, true
	if was != claudeauth.StateFailed {
		e.Since = now
	}
	a.saveClaudeBookLocked(ctx)
	a.claude.mu.Unlock()
	a.claudeEpisode(ctx, id, detail)
}

// ---- dispatch gating ----

// claudeBlocks reports whether foremanID must not run spec now: its Claude
// auth is failed and the job would run the Claude CLI there.
func (a *App) claudeBlocks(ctx context.Context, spec wire.JobSpec, foremanID string) bool {
	a.claude.mu.Lock()
	defer a.claude.mu.Unlock()
	e := a.claudeBookLocked(ctx).Foremen[foremanKey(foremanID)]
	if e == nil || e.State != claudeauth.StateFailed {
		return false
	}
	var tiers []string
	if e.Reported {
		tiers = e.Tiers
	}
	return claudeauth.RunsClaude(spec.Model, spec.Tier, tiers)
}

// claudeHold decides whether a new or held job must wait for Claude auth:
// a pinned job waits while its pin is blocked; a matched job waits when
// the deterministic pick is blocked (create_job would dispatch to it) or
// when no unblocked foreman matches. Errors never hold.
func (a *App) claudeHold(ctx context.Context, spec wire.JobSpec, release bool) (bool, string) {
	if pin := spec.Foreman; pin != "" {
		if a.claudeBlocks(ctx, spec, pin) {
			return true, claudeHoldReason(foremanKey(pin))
		}
		return false, ""
	}
	if a.matcher == nil {
		return false, ""
	}
	f, ok, err := a.matcher.Match(ctx, spec.Requires)
	if err != nil || !ok || !a.claudeBlocks(ctx, spec, f.ID) {
		return false, ""
	}
	if release {
		// The pending picker matches around blocked foremen: release when
		// some other matching foreman can take the job.
		if _, ok, err := a.matcher.MatchExcept(ctx, spec.Requires, func(g registry.Foreman) bool {
			return a.claudeBlocks(ctx, spec, g.ID)
		}); err == nil && ok {
			return false, ""
		}
	}
	return true, claudeHoldReason(foremanKey(f.ID))
}

// ---- operator flow ----

// claudeEpisode makes sure the operator is asked: one open task names
// every affected foreman and carries a login URL.
func (a *App) claudeEpisode(ctx context.Context, id, detail string) {
	if a.tasks == nil {
		return
	}
	a.claude.mu.Lock()
	b := a.claudeBookLocked(ctx)
	taskID := b.TaskID
	if taskID != "" {
		if t, err := a.tasks.GetTask(ctx, taskID); err != nil || t.Status != tasks.StatusOpen {
			taskID = ""
		}
	}
	for _, f := range b.Announced {
		if f == id && taskID != "" {
			a.claude.mu.Unlock()
			return // already asked about this foreman in the open task
		}
	}
	created := false
	if taskID == "" {
		t, err := a.tasks.CreateTask(ctx, "Claude login needed: "+id, claudeAuthTaskPriority)
		if err != nil {
			a.claude.mu.Unlock()
			log.Printf("factoryapp: claude auth: open task: %v", err)
			return
		}
		taskID, created = t.ID, true
		b.TaskID, b.Announced = t.ID, nil
	}
	b.Announced = append(b.Announced, id)
	a.saveClaudeBookLocked(ctx)
	a.claude.mu.Unlock()

	flag := true
	if _, err := a.tasks.UpdateTask(ctx, taskID, tasks.TaskUpdate{NeedsOperator: &flag}); err != nil {
		log.Printf("factoryapp: claude auth: flag task %s: %v", taskID, err)
	}
	if !created {
		a.claudeSay(ctx, taskID, fmt.Sprintf("%s is also not authenticated to the Claude CLI (%s). The same fix covers it: no separate action needed.", id, claudeDetail(detail)))
		return
	}
	a.claudeSay(ctx, taskID, fmt.Sprintf(
		"The Claude CLI on %s is not authenticated (%s), so claude jobs cannot run there. "+
			"Affected jobs are held - no retries are burned - and will run automatically once auth is back.\n\n"+
			"One action fixes every foreman: a long-lived (1-year) token is created, stored in the factory's credentials store as %q and handed to all foremen. "+
			"Preparing a sign-in link now...", id, claudeDetail(detail), claudeauth.SecretName))
	a.raiseSystemAlertForTask(ctx, claudeAuthTaskPriority, "Claude login needed: "+id,
		"The Claude CLI on "+id+" is not logged in; claude jobs there are held. Open the task for the sign-in link.", claudeAuthSource, taskID)
	a.claude.async(func() { a.claudeNewLink(context.Background(), taskID) })
}

// claudeNewLink starts a fresh setup-token session and posts its URL (or
// the manual fallback when the headless flow is unavailable here).
func (a *App) claudeNewLink(ctx context.Context, taskID string) {
	a.claude.mu.Lock()
	if old := a.claude.setup; old != nil {
		a.claude.setup = nil
		defer old.Close()
	}
	a.claude.mu.Unlock()
	sctx, cancel := context.WithTimeout(ctx, claudeSetupStartTimeout)
	defer cancel()
	s, url, err := a.claude.startSetup(sctx)
	if err != nil {
		log.Printf("factoryapp: claude auth: setup-token session: %v", err)
		a.claudeSay(ctx, taskID, "I could not start a sign-in session on the factory host ("+err.Error()+").\n\n"+
			"Fallback: on any machine with the Claude CLI, run `claude setup-token`, sign in, and paste the resulting token (it starts with sk-ant-oat) here as a reply. "+
			"The factory consumes it without storing it in this thread.")
		return
	}
	a.claude.mu.Lock()
	a.claude.setup = s
	a.claude.mu.Unlock()
	a.claudeSay(ctx, taskID, "To restore Claude auth:\n"+
		"1. Open this link and sign in with the Claude account the factory should use:\n"+url+"\n"+
		"2. Copy the code the page shows and paste it here as a reply (just the code).\n\n"+
		"The factory consumes your reply without storing it in this thread, verifies the token, stores it and pushes it to every foreman. "+
		"Alternatively paste a token from `claude setup-token` run anywhere. Reply \"new link\" if this link expires.")
}

// claudeSay posts a factory note (brain.reply: visible, no planner wake).
func (a *App) claudeSay(ctx context.Context, taskID, text string) {
	if taskID == "" {
		log.Printf("factoryapp: claude auth: %s", text)
		return
	}
	data, _ := json.Marshal(map[string]string{"text": text})
	if _, err := a.postToTask(ctx, taskID, wire.MessageEnvelope{Type: tasks.TypeBrainReply, From: "factory", Payload: data}); err != nil {
		log.Printf("factoryapp: claude auth: post to %s: %v", taskID, err)
	}
}

// interceptClaudeChat consumes an operator chat that carries a Claude
// token (anywhere), or a login code / "new link" on the open re-login
// task, so the secret never lands in a thread or wakes the planner. It
// returns the receipt envelope and task id when it handled the message.
func (a *App) interceptClaudeChat(ctx context.Context, text, taskID string) (wire.MessageEnvelope, string, bool) {
	if a.claude == nil || a.tasks == nil {
		return wire.MessageEnvelope{}, "", false
	}
	token, hasToken := claudeauth.FindToken(text)
	a.claude.mu.Lock()
	b := a.claudeBookLocked(ctx)
	authTask := b.TaskID
	setup := a.claude.setup
	a.claude.mu.Unlock()
	onAuthTask := taskID != "" && taskID == authTask
	trimmed := strings.TrimSpace(text)
	var kind string
	switch {
	case hasToken:
		kind = "token"
	case onAuthTask && codeRe.MatchString(trimmed):
		kind = "code"
	case onAuthTask && newLinkRe.MatchString(trimmed):
		kind = "link"
	default:
		return wire.MessageEnvelope{}, "", false
	}
	if taskID == "" {
		taskID = authTask
	}
	if taskID == "" {
		t, err := a.tasks.CreateTask(ctx, "Claude login", claudeAuthTaskPriority)
		if err != nil {
			log.Printf("factoryapp: claude auth: open task for token: %v", err)
			return wire.MessageEnvelope{}, "", false
		}
		taskID = t.ID
	}
	receipt := map[string]string{
		"token": "Received a Claude token from you (not stored in this thread). Verifying it...",
		"code":  "Received your login code (not stored in this thread). Exchanging it for a long-lived token...",
		"link":  "Starting a new sign-in session...",
	}[kind]
	data, _ := json.Marshal(map[string]string{"text": receipt})
	posted, err := a.postToTask(ctx, taskID, wire.MessageEnvelope{Type: tasks.TypeBrainReply, From: "factory", Payload: data})
	if err != nil {
		log.Printf("factoryapp: claude auth: receipt to %s: %v", taskID, err)
		return wire.MessageEnvelope{}, "", false
	}
	// The operator answered: the ask is spent until the outcome says
	// otherwise.
	_ = a.tasks.ClearNeedsOperator(ctx, taskID)
	a.publishTaskChanged(ctx, taskID)
	switch kind {
	case "token":
		a.claude.async(func() { a.applyClaudeToken(context.Background(), taskID, token) })
	case "code":
		a.claude.async(func() { a.redeemClaudeCode(context.Background(), taskID, setup, trimmed) })
	case "link":
		a.claude.async(func() { a.claudeNewLink(context.Background(), taskID) })
	}
	return posted, taskID, true
}

func hasClaudeToken(text string) bool {
	_, ok := claudeauth.FindToken(text)
	return ok
}

// redeemClaudeCode types the code into the waiting setup session.
func (a *App) redeemClaudeCode(ctx context.Context, taskID string, s setupHandle, code string) {
	if !a.claudeBeginWork() {
		a.claudeSay(ctx, taskID, "A previous code or token is still being processed; wait for its outcome.")
		return
	}
	defer a.claudeEndWork()
	if s == nil {
		a.claudeSay(ctx, taskID, "There is no sign-in session waiting for a code (it expired or the factory restarted). Starting a new one - use the new link below.")
		a.claudeNewLink(ctx, taskID)
		return
	}
	if s.Expired(a.clock()) {
		a.claudeSay(ctx, taskID, "That sign-in session has expired. Starting a new one...")
		a.claudeNewLink(ctx, taskID)
		return
	}
	token, err := s.Submit(ctx, code, claudeCodeTimeout)
	a.claude.mu.Lock()
	if a.claude.setup == s {
		a.claude.setup = nil // a session is single-use
	}
	a.claude.mu.Unlock()
	s.Close()
	if err != nil {
		a.claudeSay(ctx, taskID, "The code was not accepted: "+claudeauth.Redact(err.Error(), code)+"\nStarting a new sign-in session...")
		a.claudeNewLink(ctx, taskID)
		return
	}
	a.applyClaudeTokenLocked(ctx, taskID, token)
}

// applyClaudeToken verifies and stores an operator-submitted token.
func (a *App) applyClaudeToken(ctx context.Context, taskID, token string) {
	if !a.claudeBeginWork() {
		a.claudeSay(ctx, taskID, "A previous code or token is still being processed; wait for its outcome.")
		return
	}
	defer a.claudeEndWork()
	a.applyClaudeTokenLocked(ctx, taskID, token)
}

func (a *App) applyClaudeTokenLocked(ctx context.Context, taskID, token string) {
	out := a.claude.prober.Probe(ctx, token, true)
	switch out.State {
	case claudeauth.StateFailed:
		a.claudeSay(ctx, taskID, "That token was rejected by Anthropic ("+claudeauth.Redact(out.Detail, token)+"). Nothing was stored. Reply \"new link\" for a fresh sign-in link.")
		flag := true
		_, _ = a.tasks.UpdateTask(ctx, taskID, tasks.TaskUpdate{NeedsOperator: &flag})
		return
	case claudeauth.StateUnknown:
		a.claudeSay(ctx, taskID, "Could not verify the token right now ("+claudeauth.Redact(out.Detail, token)+"); storing it anyway - each foreman re-checks it and reports here.")
	}
	if _, err := a.secrets.Put(claudeauth.SecretName, token, claudeauth.TokenEnv); err != nil {
		a.claudeSay(ctx, taskID, "Storing the token failed: "+claudeauth.Redact(err.Error(), token))
		return
	}
	a.claude.mu.Lock()
	b := a.claudeBookLocked(ctx)
	for _, e := range b.Foremen {
		e.ProbeWanted = true
	}
	if b.TaskID == "" {
		b.TaskID = taskID
	}
	pending := b.failed()
	a.saveClaudeBookLocked(ctx)
	a.claude.mu.Unlock()
	log.Printf("factoryapp: claude auth: operator token verified (%s) and stored as %s", out.State, claudeauth.SecretName)
	msg := "Token verified and stored as " + claudeauth.SecretName + " (CLAUDE_CODE_OAUTH_TOKEN). Every foreman picks it up on its next check-in (within about a minute) and re-checks its Claude auth."
	if len(pending) > 0 {
		msg += " Waiting on: " + strings.Join(pending, ", ") + "."
	}
	a.claudeSay(ctx, taskID, msg)
	if len(pending) == 0 {
		a.claudeMaybeClose(ctx)
	}
}

func (a *App) claudeBeginWork() bool {
	a.claude.mu.Lock()
	defer a.claude.mu.Unlock()
	if a.claude.busy {
		return false
	}
	a.claude.busy = true
	return true
}

func (a *App) claudeEndWork() {
	a.claude.mu.Lock()
	a.claude.busy = false
	a.claude.mu.Unlock()
}

// claudeRestored handles a foreman's failed -> ok transition: say so,
// release held jobs, and close the episode when nothing is failed.
func (a *App) claudeRestored(ctx context.Context, id string) {
	a.claudeReleased(ctx, id, "Claude auth check passed")
}

// claudeReleased releases held jobs after id left the failed state and
// tells the open task why (note).
func (a *App) claudeReleased(ctx context.Context, id, note string) {
	log.Printf("factoryapp: claude auth: %s: %s", id, note)
	a.claude.mu.Lock()
	taskID := a.claudeBookLocked(ctx).TaskID
	a.claude.mu.Unlock()
	n, err := a.ReleasePass(ctx)
	if err != nil {
		log.Printf("factoryapp: claude auth: release after %s restored: %v", id, err)
	}
	if taskID != "" {
		a.claudeSay(ctx, taskID, fmt.Sprintf("%s: %s. Released %d held job(s).", id, note, n))
	}
	a.claudeMaybeClose(ctx)
}

// claudeMaybeClose closes the episode task once no foreman is failed.
func (a *App) claudeMaybeClose(ctx context.Context) {
	a.claude.mu.Lock()
	b := a.claudeBookLocked(ctx)
	if len(b.failed()) > 0 || b.TaskID == "" {
		a.claude.mu.Unlock()
		return
	}
	taskID := b.TaskID
	b.TaskID, b.Announced = "", nil
	setup := a.claude.setup
	a.claude.setup = nil
	a.saveClaudeBookLocked(ctx)
	a.claude.mu.Unlock()
	if setup != nil {
		setup.Close()
	}
	a.claudeSay(ctx, taskID, "No foreman is failing the Claude auth check any more. Closing this task; held jobs run automatically.")
	closed, reason := tasks.StatusClosed, "claude-auth-restored"
	if _, err := a.tasks.UpdateTask(ctx, taskID, tasks.TaskUpdate{Status: &closed, CloseReason: &reason}); err != nil {
		log.Printf("factoryapp: claude auth: close %s: %v", taskID, err)
	}
	a.publishTaskChanged(ctx, taskID)
}

// claudeUnreportedRetry is how long a result-detected failure on a
// foreman that never reports claude_auth (an older build, which cannot
// confirm recovery) blocks before its jobs are tried again.
const claudeUnreportedRetry = 30 * time.Minute

// claudeAuthPass is the periodic housekeeping: an expired setup session
// is dropped (the operator gets a fresh link on "new link" or a code), and
// a failure on a foreman that cannot report recovery is retried.
func (a *App) claudeAuthPass(ctx context.Context) {
	a.claude.mu.Lock()
	var retried []string
	for id, e := range a.claudeBookLocked(ctx).Foremen {
		if e.State == claudeauth.StateFailed && !e.Reported && a.clock().Sub(e.FailedAt) >= claudeUnreportedRetry {
			e.State, e.Since = claudeauth.StateUnknown, a.clock()
			retried = append(retried, id)
		}
	}
	if len(retried) > 0 {
		a.saveClaudeBookLocked(ctx)
	}
	a.claude.mu.Unlock()
	for _, id := range retried {
		a.claudeReleased(ctx, id, fmt.Sprintf("this foreman's build does not report Claude auth health, so its held jobs are retried after %s", claudeUnreportedRetry))
	}
	a.claude.mu.Lock()
	s := a.claude.setup
	if s != nil && !a.claude.busy && s.Expired(a.clock()) {
		a.claude.setup = nil
	} else {
		s = nil
	}
	taskID := a.claudeBookLocked(ctx).TaskID
	a.claude.mu.Unlock()
	if s != nil {
		s.Close()
		a.claudeSay(ctx, taskID, "The sign-in link above has expired. Reply \"new link\" for a fresh one, or paste a token from `claude setup-token`.")
	}
}

func (a *App) claudeAuthLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			a.claude.mu.Lock()
			s := a.claude.setup
			a.claude.setup = nil
			a.claude.mu.Unlock()
			if s != nil {
				s.Close()
			}
			return
		case <-t.C:
			a.claudeAuthPass(ctx)
		}
	}
}

// ---- operator status API ----

// handleClaudeAuthStatus serves GET /v1/claude-auth: per-foreman health,
// whether a token is stored (never the value), and the open task.
func (a *App) handleClaudeAuthStatus(w http.ResponseWriter, r *http.Request) {
	_, rev := a.claudeToken()
	a.claude.mu.Lock()
	b := a.claudeBookLocked(r.Context())
	data, _ := json.Marshal(b)
	setup := a.claude.setup != nil
	a.claude.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"book":          json.RawMessage(data),
		"token_stored":  rev != "",
		"token_rev":     rev,
		"setup_pending": setup,
	})
}

// handleClaudeAuthCheck serves POST /v1/claude-auth/check: every foreman
// probes on its next check-in.
func (a *App) handleClaudeAuthCheck(w http.ResponseWriter, r *http.Request) {
	a.claude.mu.Lock()
	b := a.claudeBookLocked(r.Context())
	for _, e := range b.Foremen {
		e.ProbeWanted = true
	}
	a.saveClaudeBookLocked(r.Context())
	a.claude.mu.Unlock()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "probe requested"})
}

func claudeDetail(s string) string {
	if strings.TrimSpace(s) == "" {
		return "no detail"
	}
	return s
}
