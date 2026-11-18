// Package foremanapp wires the foreman binary (issue #21): the local inbox,
// auth, notify, routing, worker harness, and the worker + brain MCP surfaces
// composed into one HTTP mux plus the background loops (remote inbox client,
// local inbox consumer, brain queue drain, capabilities refresh).
package foremanapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/pricec/factory/internal/claudecli"

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/buildinfo"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/mcpforeman"
	"github.com/pricec/factory/internal/mcpworker"
	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/notify"
	"github.com/pricec/factory/internal/routing"
	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

// FactoryMainInbox is the factory's processing inbox: forwarded job.result
// and job.event envelopes target it (spec §7.3).
const FactoryMainInbox = "factory:main"

// Config configures the foreman App.
type Config struct {
	// FactoryURL is the factory's base URL, e.g. "http://127.0.0.1:8080"
	// (the factory's AdvertiseURL).
	FactoryURL string
	// ID is the bare foreman id, e.g. "alpha".
	ID string
	// DataDir holds inbox.db, auth.db, and paired.json.
	DataDir string
	// Addr is the foreman's own listen address, e.g. ":8090".
	Addr string
	// AdvertiseURL is logged at startup; v1 does not strictly need it
	// (the factory never calls back over it beyond the local mux).
	// Empty -> http://<hostname>:<port of Addr>.
	AdvertiseURL string
	// PairingToken is the one-time pairing token, first run only.
	// Empty with no stored paired.json -> New fails ("not paired").
	PairingToken string
	// BrainModel is the opencode model string for brain sessions;
	// "" -> the pinned GLM tiering default (spec §6.5).
	BrainModel string
	// WorkerFlashModel is the worker's cheap tier (spec §5.1
	// `tier: "flash"`, also the no-tier default);
	// "" -> the pinned GLM 5.3 Flash default (spec §6.5).
	WorkerFlashModel string
	// WorkerModel is the worker's middle tier (spec §5.1
	// `tier: "default"`); "" -> the pinned GLM 5.2 default.
	WorkerModel string
	// WorkerHardestModel is the model for `tier: "hardest"` jobs (spec
	// §5.1, §6.5); "" -> the pinned GLM 5.3 default.
	WorkerHardestModel string
	// SettingsRefresh is the role-model settings pull interval (spec
	// §6.5, 2026-11-15 amendment, settings.go); 0/negative -> 60s. A
	// test seam.
	SettingsRefresh time.Duration
	// Now is the clock; nil -> real UTC time.
	Now func() time.Time
	// ProcRoot is the process table the startup orphan probe reads (spec
	// §8.7); "" -> /proc. A test seam: tests point it at a fake tree.
	ProcRoot string
	// Build is the build id reported in the caps push and compared against
	// update adverts (spec §12); "" -> buildinfo.Current().
	Build string
}

// App is the wired foreman agent host.
type App struct {
	cfg        Config
	factoryURL string
	id         string
	inboxID    string // "foreman:{id}"
	dataDir    string
	advertise  string
	now        func() time.Time

	paired pairingRecord
	// secretBackoff is the handout fetch retry delay; zero means 1s (test seam).
	secretBackoff time.Duration
	brainToken    string

	inbox  *inbox.Service
	auth   *auth.Store
	authn  *auth.Authenticator
	hub    *notify.Hub
	router *routing.Router
	brain  *BrainRunner
	poster *Poster
	client *notify.Client
	// modelCfg is the settings-synced role-model resolution (spec §6.5,
	// settings.go): brain + worker tiers.
	modelCfg *modelConfig
	// claudeAuth is the Claude CLI auth health + factory token holder
	// (spec §13.3, claudeauth.go).
	claudeAuth *claudeAuth
	// brainInboxID is the foreman's local durable brain queue (spec §3.3,
	// 2026-11-15 amendment): the sub-inbox "<inboxID>:brain" on the same
	// inbox store. Brain-delegated envelopes are enqueued here and acked
	// from their source by the consume loop; brainQueueLoop drains it one
	// envelope at a time, so no LLM session ever blocks the hot paths.
	brainInboxID string

	// workerCtx is the lifetime context of spawned workers. Spawns arrive
	// on request-scoped contexts (the mcpforeman spawn_worker HTTP handler,
	// the notify.Client batch handler), and net/http cancels those the
	// moment the call returns - a worker that inherited one died instantly
	// on a canceled context while the dispatch looked successful. Workers
	// run under this context instead; it ends only when the App stops
	// (Close).
	workerCtx context.Context
	// workerCancel ends workerCtx.
	workerCancel context.CancelFunc

	// childMu guards the child-spawn state (spec §9.2, 2026-11-15
	// sub-spawning amendment). childParent maps each spawned child job id
	// to its parent job id: the consumer copies the child's terminal
	// job.result into the parent's per-job inbox job:{parent}, where the
	// parent collects it via collect_children. childrenOf maps each parent
	// job id to the children spawned in its CURRENT attempt - reset when a
	// fresh attempt of the same job id spawns (a retried parent must never
	// collect a previous attempt's results); each spawn_child adds to it.
	childMu     sync.Mutex
	childParent map[string]string
	childrenOf  map[string]map[string]bool

	// spawnedMu guards spawned: every job id this process started a worker
	// for (spec §8.7). The startup orphan pass never fails these.
	spawnedMu sync.Mutex
	spawned   map[string]bool
	// live is the subset of spawned whose harness goroutine has not yet
	// returned (guarded by spawnedMu). A live job's credential is revoked
	// when its harness exits, not when its job.result is forwarded: the
	// harness posts job.usage after the agent child exits, which can be
	// after report_result.
	live map[string]bool
	// cancels holds the stop channel of each live worker; cancelled is every
	// job id a job.cancel was seen for (both guarded by spawnedMu; cancel.go).
	cancels   map[string]chan struct{}
	cancelled map[string]bool
	// procRoot is the process table the orphan probe reads (spec §8.7).
	procRoot string

	// BuildHarnessConfig builds the workerharness.Config the spawner runs
	// for one job. A field so tests can replace the opencode binary and
	// URLs; the default derives them from cfg (see defaultHarnessConfig).
	BuildHarnessConfig func(spec wire.JobSpec, bearer string) workerharness.Config

	// Auto-update (spec §12): build is the running build id; updater is
	// the self-updater fed by the caps push; gate is the drain gate on
	// dispatch intake and the live-work count; exitCh carries the exit code
	// the updater asks for (cmd/foreman exits with it); listenAddr is the
	// bound listener the confirm self-check dials.
	build      string
	updater    *Updater
	gate       *intakeGate
	exitCh     chan int
	listenMu   sync.Mutex
	listenAddr string

	handler http.Handler
}

// New opens the local stores, pairs with the factory when needed (spec
// §7.1), wires the router/spawner/brain/poster/client, and builds the local
// /v1 mux. It starts no goroutines and no listeners; Run owns the loops.
func New(cfg Config) (*App, error) {
	switch {
	case cfg.FactoryURL == "":
		return nil, errors.New("foremanapp: Config.FactoryURL is required")
	case cfg.ID == "":
		return nil, errors.New("foremanapp: Config.ID is required")
	case cfg.DataDir == "":
		return nil, errors.New("foremanapp: Config.DataDir is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8090"
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("foremanapp: data dir: %w", err)
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	// Model tiering (spec §6.5): unset fields fall back to the pinned GLM
	// defaults; callers override via env (cmd/foreman). Resolved BEFORE
	// the App literal copies cfg - every later construction (BrainRunner,
	// spawner) must see the resolved values.
	cfg.BrainModel = models.Resolve(cfg.BrainModel, models.ForemanBrainDefault)
	cfg.WorkerFlashModel = models.Resolve(cfg.WorkerFlashModel, models.WorkerFlash)
	cfg.WorkerModel = models.Resolve(cfg.WorkerModel, models.WorkerStandard)
	cfg.WorkerHardestModel = models.Resolve(cfg.WorkerHardestModel, models.WorkerHardest)

	a := &App{
		cfg:         cfg,
		factoryURL:  cfg.FactoryURL,
		id:          cfg.ID,
		inboxID:     "foreman:" + cfg.ID,
		dataDir:     cfg.DataDir,
		advertise:   cfg.AdvertiseURL,
		now:         now,
		childParent: map[string]string{},
		childrenOf:  map[string]map[string]bool{},
		spawned:     map[string]bool{},
		live:        map[string]bool{},
		cancels:     map[string]chan struct{}{},
		cancelled:   map[string]bool{},
		procRoot:    cfg.ProcRoot,
		build:       cfg.Build,
		gate:        &intakeGate{},
		exitCh:      make(chan int, 1),
	}
	if a.procRoot == "" {
		a.procRoot = defaultProcRoot
	}
	if a.build == "" {
		a.build = buildinfo.Current()
	}
	if a.advertise == "" {
		a.advertise = defaultAdvertiseURL(cfg.Addr)
	}

	// Worker lifetime context: created in New (a spawn may arrive as soon
	// as the mux serves) and canceled in Close.
	a.workerCtx, a.workerCancel = context.WithCancel(context.Background())

	// Stores: open both or none.
	var err error
	a.inbox, err = inbox.Open(filepath.Join(cfg.DataDir, "inbox.db"))
	if err != nil {
		return nil, fmt.Errorf("foremanapp: open inbox: %w", err)
	}
	a.auth, err = auth.Open(filepath.Join(cfg.DataDir, "auth.db"))
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("foremanapp: open auth: %w", err)
	}
	a.auth.Now = now

	// Pairing (spec §7.1): load paired.json, or burn the one-time token.
	ctx := context.Background()
	a.paired, err = loadOrPair(ctx, a)
	if err != nil {
		_ = a.Close()
		return nil, err
	}
	if a.paired.InboxID != "" {
		a.inboxID = a.paired.InboxID
	}
	// The brain queue rides the foreman's own inbox id (spec §3.3,
	// 2026-11-15 amendment) - compute it after the pairing override so a
	// stored pairing's inbox id wins.
	a.brainInboxID = a.inboxID + ":brain"

	// Brain credential: a local foreman:{id} credential on the foreman's
	// own auth store, valid for its own inbox. The factory-paired bearer
	// lives in the factory's auth DB and cannot authenticate against the
	// local mcpforeman/inbox surfaces, so the brain token is minted
	// locally via the same PairForeman path (spec §7.1, §7.3).
	localPair, _, err := a.auth.IssuePairingToken(ctx)
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("foremanapp: issue local pairing token: %w", err)
	}
	a.brainToken, _, err = a.auth.PairForeman(ctx, localPair, a.id)
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("foremanapp: mint brain credential: %w", err)
	}

	// Local SSE fan-out (spec §4.4) + inbox auth.
	a.hub = notify.NewHub()
	a.hub.AttachTo(a.inbox)
	a.authn = auth.NewAuthenticator(a.auth)

	// Brain, poster, spawner, router (spec §3.3). The brain's WorkDir is
	// its own subdirectory: the generated opencode.json is the V2
	// project config discovered from the working dir, so it must not sit
	// in DataDir itself, which is an ancestor of every worker job dir
	// (ancestor configs would leak into worker sessions).
	// The model config holder (spec §6.5, 2026-11-15 amendment) seeds
	// from the env-resolved cfg and is updated by the settings pull
	// (settings.go) - the brain resolves its model per session via
	// ModelFn and the spawner per job, so panel changes apply without a
	// restart. Precedence per role: panel > env > pinned default.
	a.modelCfg = newModelConfig(modelValues{
		brain:    cfg.BrainModel,
		flash:    cfg.WorkerFlashModel,
		standard: cfg.WorkerModel,
		hardest:  cfg.WorkerHardestModel,
	})
	a.claudeAuth = newClaudeAuth()
	a.brain = &BrainRunner{
		ExtraEnv: a.claudeAuth.Env,
		Model:    cfg.BrainModel,
		ModelFn:  a.modelCfg.Brain,
		MCPURL:   localBaseURL(cfg.Addr) + "/v1/mcp/foreman",
		Bearer:   a.brainToken,
		WorkDir:  filepath.Join(cfg.DataDir, "brain"),
	}
	a.poster = &Poster{FactoryURL: cfg.FactoryURL, Bearer: a.paired.Bearer}
	a.brain.OnUsage = func(ctx context.Context, u claudecli.Usage) {
		ev := wire.UsageEvent{
			Kind: wire.UsageKindBrain, ForemanID: cfg.ID, Model: u.Model, Input: u.Input, Output: u.Output,
			CacheCreate: u.CacheCreate, CacheRead: u.CacheRead, CostUSD: u.CostUSD, SessionID: u.SessionID, Limit: u.Limit, RateLimit: u.RateLimit,
			DurationMs: u.DurationMs,
		}
		if ev.Model == "" {
			ev.Model = a.modelCfg.Brain()
		}
		payload, err := json.Marshal(ev)
		if err != nil {
			return
		}
		if err := a.poster.PostToParent(ctx, wire.MessageEnvelope{Type: wire.TypeUsageRecord, From: a.inboxID, Payload: payload}); err != nil {
			log.Printf("foremanapp: post brain usage: %v", err)
		}
	}
	a.BuildHarnessConfig = a.defaultHarnessConfig
	// The brain reporter wraps the runner so a failed session posts
	// brain.failed upstream (spec §3.2): without it a dead delegation acks
	// silently and the operator waits forever.
	a.router = &routing.Router{
		Brain:   &brainReporter{inner: a.brain, poster: a.poster, from: a.inboxID},
		Poster:  a.poster,
		Spawner: spawnerFunc(a.SpawnWorker),
	}

	// MCP surfaces (spec §9.2, §9.3). The worker surface's spawn_child and
	// collect_children (2026-11-15 sub-spawning amendment) share the
	// foreman's Spawn seam and read the spawn state below.
	workerH, err := mcpworker.Handler(mcpworker.Config{
		Auth:           a.auth,
		Inbox:          a.inbox,
		ForemanInboxID: a.inboxID,
		WorkDir:        cfg.DataDir,
		Now:            now,
		Spawn:          a.SpawnWorker,
		ParentSpec:     a.parentSpec,
		ChildJobs:      a.childrenOfJob,
		RequestMerge:   a.requestMerge, // job-owned merges (spec §6.11)
	})
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("foremanapp: worker MCP: %w", err)
	}
	foremanH, err := mcpforeman.Handler(mcpforeman.Config{
		Auth:           a.auth,
		Inbox:          a.inbox,
		ForemanInboxID: a.inboxID,
		ParentInboxID:  FactoryMainInbox,
		Spawn:          a.SpawnWorker,
	})
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("foremanapp: foreman MCP: %w", err)
	}

	// Local mux: the foreman's own inbox backs foreman:{id} and job:{id}
	// inboxes (workers post results/questions here).
	mux := http.NewServeMux()
	mux.Handle("/v1/inboxes/", inbox.NewHandler(a.inbox, a.authn))
	mux.Handle("GET /v1/inboxes/{id}/stream", notify.StreamHandler(a.hub, a.authn))
	mux.Handle("/v1/mcp/worker", workerH)
	mux.Handle("/v1/mcp/foreman", foremanH)
	a.handler = mux

	// Remote inbox client (spec §4.5): streams the factory-side copy of
	// foreman:{id}; batches are routed and acked via the factory's REST.
	a.client = &notify.Client{
		BaseURL: cfg.FactoryURL,
		InboxID: a.inboxID,
		Token:   a.paired.Bearer,
		Handler: a.handleRemoteBatch,
	}

	// Self-updater (spec §12): fed by the caps push, drains via the gate.
	a.updater = &Updater{
		DataDir:    cfg.DataDir,
		Running:    a.build,
		Platform:   runtime.GOOS + "-" + runtime.GOARCH,
		FactoryURL: cfg.FactoryURL,
		Bearer:     a.paired.Bearer,
		Now:        now,
		Exit:       a.requestExit,
		Busy:       a.gate.busy,
		Pause:      a.gate.pause,
		Resume:     a.resumeIntake,
		SelfCheck:  a.selfCheck,
	}
	return a, nil
}

// Handler returns the local /v1 mux.
func (a *App) Handler() http.Handler { return a.handler }

// Paired reports whether the foreman carries a stored pairing.
func (a *App) Paired() bool { return a.paired.Bearer != "" }

// AdvertiseURL returns the foreman's advertised base URL (logged at
// startup; the factory does not call back over it in v1).
func (a *App) AdvertiseURL() string { return a.advertise }

// Inbox returns the local inbox service (test/integration accessor).
func (a *App) Inbox() *inbox.Service { return a.inbox }

// Auth returns the local auth store (test/integration accessor).
func (a *App) Auth() *auth.Store { return a.auth }

// Router returns the wired router (test/integration accessor).
func (a *App) Router() *routing.Router { return a.router }

// BrainToken returns the local foreman brain bearer, valid on the foreman's
// own mux (brain MCP + own-inbox read/post). Test accessor; also the token
// the default BrainRunner.Exec injects as FOREMAN_BRAIN_TOKEN.
func (a *App) BrainToken() string { return a.brainToken }

// Brain returns the wired brain runner (test seam: replace its Exec).
func (a *App) Brain() *BrainRunner { return a.brain }

// BrainInboxID returns the local durable brain queue inbox id
// ("<inboxID>:brain", spec §3.3 2026-11-15 amendment). Test/integration
// accessor: the queue is internal - only handleBatch enqueues into it and
// brainQueueLoop drains it.
func (a *App) BrainInboxID() string { return a.brainInboxID }

// Run runs the background loops: the remote inbox client, the local inbox
// consumer, the brain queue drain, the parent forwarder, and the
// capabilities refresh (startup push + 60s ticker), and the role-model
// settings pull (startup + 60s, settings.go). It blocks until ctx is
// done. v1 fails fast: a definitive remote-loop error is logged and the
// loop ends.
func (a *App) Run(ctx context.Context) error {
	// Orphan recovery (spec §8.7) runs BEFORE any loop starts, so no spawn
	// can race the registry reconciliation: a freshly started foreman has no
	// live worker children, and jobs its previous process left 'running' are
	// failed (crash) here so their retries re-arm.
	a.recoverOrphans(ctx)
	// Auto-update boot step (spec §12.5): a leftover marker is confirmed
	// (after the first successful caps push), discarded, or blocklisted.
	a.updater.Boot()
	if err := a.pushCapabilities(ctx); err != nil {
		log.Printf("foremanapp: initial capabilities push: %v", err)
	}
	var wg sync.WaitGroup
	wg.Add(8)
	go func() {
		defer wg.Done()
		a.claudeAuthLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		a.updater.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		if err := a.client.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("foremanapp: inbox client stopped: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		a.capsLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		a.localLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		a.brainQueueLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		a.parentLoop(ctx)
	}()
	go func() {
		defer wg.Done()
		a.settingsLoop(ctx)
	}()
	wg.Wait()
	return nil
}

// Close stops the App: spawned workers are ended (their lifetime context)
// and the local stores close.
func (a *App) Close() error {
	if a.workerCancel != nil {
		a.workerCancel()
	}
	var errs []error
	if a.inbox != nil {
		errs = append(errs, a.inbox.Close())
	}
	if a.auth != nil {
		errs = append(errs, a.auth.Close())
	}
	return errors.Join(errs...)
}

// handleRemoteBatch routes one factory-side snapshot batch and acks the
// handled ids via the factory's REST API (the notify.Client Handler
// contract: the caller owns processing AND acking; Run propagates its
// error, so an ack failure ends the loop - fail fast in v1).
func (a *App) handleRemoteBatch(ctx context.Context, envs []wire.MessageEnvelope) error {
	return a.handleBatch(ctx, envs, a.ackRemote)
}

// handleBatch routes each envelope through the rule table (spec §3.3),
// acks the successfully handled ids, and revokes the worker credential of
// any terminal job.result observed (spec §7.4 analog: the result IS the
// terminal signal the foreman sees). Brain-delegated envelopes (spec §3.3,
// 2026-11-15 amendment) are the exception: they are handed to the local
// durable brain queue (enqueueBrain) and acked on successful enqueue, so
// the consume loop - and with it the hot paths job.request -> spawn_worker
// and job.result -> forward - never waits on an LLM session.
func (a *App) handleBatch(ctx context.Context, envs []wire.MessageEnvelope, ack func(context.Context, []string) error) error {
	var ids []string
	for _, env := range envs {
		// Auto-update drain (spec §12.4): while intake is paused a NEW
		// dispatch stays unacked in its inbox (redelivered on resume or
		// after the restart); admitted ones count as live work until
		// their spawn has registered the worker.
		if env.Type == wire.TypeJobCancel {
			a.cancelWorker(env.JobID)
			ids = append(ids, env.ID)
			continue
		}
		if env.Type == "job.request" {
			if a.jobCancelled(env.JobID) {
				log.Printf("foremanapp: job.request %s for cancelled job %s dropped", env.ID, env.JobID)
				ids = append(ids, env.ID)
				continue
			}
			if !a.gate.beginDispatch() {
				log.Printf("foremanapp: job.request %s deferred: draining for auto-update (not acked; redelivered)", env.ID)
				continue
			}
			err := a.router.Handle(ctx, env)
			a.gate.endDispatch()
			if err != nil {
				log.Printf("foremanapp: route %s %s: %v (not acked; redelivered)", env.Type, env.ID, err)
				continue
			}
			ids = append(ids, env.ID)
			continue
		}
		if routing.DelegatesToBrain(env.Type) {
			if err := a.enqueueBrain(ctx, env); err != nil {
				log.Printf("foremanapp: brain queue %s %s: %v (not acked; redelivered)", env.Type, env.ID, err)
				continue
			}
			ids = append(ids, env.ID)
			continue
		}
		// Child-result copy (spec §9.2, 2026-11-15 sub-spawning amendment):
		// a terminal child job.result is copied into the parent's per-job
		// inbox BEFORE the factory forward, so a copy failure redelivers
		// the whole envelope and nothing is acked early. The copy is
		// idempotent (the inbox dedupes job.result per inbox + job id).
		if env.Type == "job.result" {
			if err := a.copyChildResult(ctx, env); err != nil {
				log.Printf("foremanapp: child result copy %s: %v (not acked; redelivered)", env.ID, err)
				continue
			}
		}
		if err := a.router.Handle(ctx, env); err != nil {
			log.Printf("foremanapp: route %s %s: %v (not acked; redelivered)", env.Type, env.ID, err)
			continue
		}
		ids = append(ids, env.ID)
		if env.Type == "job.result" && env.JobID != "" {
			// A job whose harness is still running keeps its credential
			// until the harness exits (runWorker revokes then): the
			// harness's post-exit job.usage needs it. Other jobs (remote,
			// orphan, no local harness) are revoked right away.
			if !a.workerLive(env.JobID) {
				a.revokeJobCredential(ctx, env.JobID)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	return ack(ctx, ids)
}

// copyChildResult routes one terminal child job.result to its parent's
// per-job inbox job:{parent} (spec §9.2, 2026-11-15 sub-spawning
// amendment): the parent worker collects child results itself via
// collect_children - the envelope payload stays byte-identical (§3.4) and
// never detours through the foreman brain. Only children this process
// spawned are copied (childParent state; a foreman restart empties it, and
// with it every child it spawned is gone too). The §8.5 synthesized
// spawn-failure results flow through the same path, so a harness failure
// is loud at the parent, not only at the factory. Duplicate posts
// (redelivery after copy-but-crash-before-ack) are idempotent success: the
// inbox dedupes job.result per (inbox, job_id).
func (a *App) copyChildResult(ctx context.Context, env wire.MessageEnvelope) error {
	parent, ok := a.childParentOf(env.JobID)
	if !ok {
		return nil // not a spawned child
	}
	_, err := a.inbox.Post(ctx, "job:"+parent, wire.MessageEnvelope{
		Type:    env.Type,
		JobID:   env.JobID,
		From:    env.From,
		Payload: env.Payload,
	})
	var dup *inbox.DuplicateError
	if errors.As(err, &dup) {
		return nil
	}
	return err
}

// recordSpawnState tracks the child-spawn state the §9.2 sub-spawning
// amendment runs on (called by SpawnWorker before the harness starts, so
// the state always precedes any possible report). A fresh (non-child)
// spawn opens a new attempt generation: its child set starts empty, so a
// retried parent never collects a previous attempt's copies. A child spawn
// is recorded under its parent for both the consumer copy (childParent)
// and the collect_children filter (childrenOf).
func (a *App) recordSpawnState(spec wire.JobSpec) {
	a.childMu.Lock()
	defer a.childMu.Unlock()
	if spec.ParentJobID == "" {
		a.childrenOf[spec.ID] = map[string]bool{}
		return
	}
	a.childParent[spec.ID] = spec.ParentJobID
	set, ok := a.childrenOf[spec.ParentJobID]
	if !ok {
		set = map[string]bool{}
		a.childrenOf[spec.ParentJobID] = set
	}
	set[spec.ID] = true
}

// childParentOf returns the parent job id of a spawned child, if any.
func (a *App) childParentOf(jobID string) (string, bool) {
	a.childMu.Lock()
	defer a.childMu.Unlock()
	parent, ok := a.childParent[jobID]
	return parent, ok
}

// childrenOfJob returns the child job ids the given parent spawned in its
// current attempt (the mcpworker collect_children filter seam, spec §9.2).
func (a *App) childrenOfJob(parent string) []string {
	a.childMu.Lock()
	defer a.childMu.Unlock()
	set := a.childrenOf[parent]
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// parentSpec reads a job's persisted JobSpec (the mcpworker spawn_child
// seam, spec §9.2): the spawner writes jobs/{id}/job.json before the
// harness starts, so a running job always has one. Normalize refills
// fields left at zero so inheritance caps compare against applied values.
func (a *App) parentSpec(jobID string) (wire.JobSpec, bool, error) {
	data, err := os.ReadFile(filepath.Join(a.dataDir, jobsDirName, jobID, "job.json"))
	if errors.Is(err, os.ErrNotExist) {
		return wire.JobSpec{}, false, nil
	}
	if err != nil {
		return wire.JobSpec{}, false, err
	}
	var spec wire.JobSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return wire.JobSpec{}, false, fmt.Errorf("job spec %s: %w", jobID, err)
	}
	spec.Normalize()
	return spec, true, nil
}

// enqueueBrain durably hands one brain-delegated envelope to the brain
// queue (spec §3.3, 2026-11-15 amendment): the copy keeps type, job id,
// sender, and payload verbatim - only the id and ts are re-assigned by the
// store, exactly like any other inbox post. The caller acks the source
// copy only when this succeeds; a failure leaves it unacked and
// redelivered (at-least-once, §4).
func (a *App) enqueueBrain(ctx context.Context, env wire.MessageEnvelope) error {
	posted, err := a.inbox.Post(ctx, a.brainInboxID, wire.MessageEnvelope{
		Type:    env.Type,
		JobID:   env.JobID,
		From:    env.From,
		Payload: env.Payload,
	})
	if err != nil {
		return err
	}
	log.Printf("foremanapp: brain queue %s %s queued as %s (async)", env.Type, env.ID, posted.ID)
	return nil
}

// brainQueueLoop drains the local durable brain queue one envelope at a
// time (spec §3.3, 2026-11-15 amendment): each envelope runs a full brain
// session - bounded by the brain runner's own session timeout - and the
// ack lands only after the session returns, so a crash mid-session leaves
// the queue copy unacked and redelivered on restart (at-least-once, no
// loss). Handling one envelope per Get iteration and acking before the
// next Get means an in-flight envelope is never re-read within a process:
// no double-handling. Brain failures are logged inside the router (and
// reported upstream by any brain-failure wrapper wired as Router.Brain)
// and never surface as errors here - a poison envelope is consumed, not
// redelivered forever, so it cannot block later brain work.
func (a *App) brainQueueLoop(ctx context.Context) {
	for {
		envs, err := a.inbox.Get(ctx, a.brainInboxID, 1, 5*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("foremanapp: brain queue: get: %v", err)
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		if len(envs) == 0 {
			continue
		}
		env := envs[0]
		// Auto-update drain (spec §12.4): no new brain session starts
		// while intake is paused; the copy stays queued.
		if !a.gate.beginBrain() {
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		// The full rule table applies, but handleBatch only ever
		// enqueues brain-delegated types, so this always lands on the
		// brain action (job.question keeps its 60s SLA there).
		err = a.router.Handle(ctx, env)
		a.gate.endBrain()
		if err != nil {
			// Unreachable for brain-delegated types (the router logs
			// brain errors and returns nil); kept at-least-once:
			// leave unacked and retry rather than drop.
			log.Printf("foremanapp: brain queue: route %s %s: %v (redelivered)", env.Type, env.ID, err)
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		if ctx.Err() != nil {
			// Shutdown interrupted the session (the router logged the
			// canceled-session error): leave the copy unacked so the
			// next start redelivers it - an interrupted session never
			// consumes its envelope (at-least-once, no loss).
			log.Printf("foremanapp: brain queue: %s %s interrupted by shutdown (redelivered)", env.Type, env.ID)
			return
		}
		if err := a.inbox.Ack(ctx, a.brainInboxID, []string{env.ID}); err != nil {
			log.Printf("foremanapp: brain queue: ack %s: %v (redelivered)", env.ID, err)
		}
	}
}

// localLoop drains the foreman's own local inbox via the library API (no
// HTTP): job.result and job.question posted by in-process workers, plus
// brain.wake posted locally. Hot-path envelopes are routed inline and
// acked locally; brain-delegated ones are handed to the durable brain
// queue (handleBatch, spec §3.3 2026-11-15 amendment) so this loop never
// waits on an LLM session.
func (a *App) localLoop(ctx context.Context) {
	for {
		envs, err := a.inbox.Get(ctx, a.inboxID, 50, 5*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("foremanapp: local consumer: get: %v", err)
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		if len(envs) == 0 {
			continue
		}
		if err := a.handleBatch(ctx, envs, a.ackLocal); err != nil {
			log.Printf("foremanapp: local consumer: %v", err)
		}
		// A deferred (draining) envelope stays unacked and Get returns it
		// at once: back off instead of spinning.
		if a.gate.isPaused() && !sleepCtx(ctx, time.Second) {
			return
		}
	}
}

// parentLoop drains the foreman's local copy of factory:main (spec §7.3):
// a staging inbox for envelopes bound for the factory - the brain's
// post_to_factory output and anything else addressed there. Every
// envelope is forwarded verbatim to the parent over HTTP and acked
// locally; the §3.3 rule table never applies here (the sender already
// chose the destination, and routing brain replies through it would loop
// them straight back into the brain).
func (a *App) parentLoop(ctx context.Context) {
	for {
		envs, err := a.inbox.Get(ctx, FactoryMainInbox, 50, 5*time.Second)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Printf("foremanapp: parent forwarder: get: %v", err)
			if !sleepCtx(ctx, time.Second) {
				return
			}
			continue
		}
		if len(envs) == 0 {
			continue
		}
		acked := make([]string, 0, len(envs))
		for _, env := range envs {
			if err := a.poster.PostToParent(ctx, env); err != nil {
				log.Printf("foremanapp: parent forwarder: %s %s: %v (redelivered)", env.Type, env.ID, err)
				continue
			}
			acked = append(acked, env.ID)
		}
		if len(acked) == 0 {
			continue
		}
		if err := a.inbox.Ack(ctx, FactoryMainInbox, acked); err != nil {
			log.Printf("foremanapp: parent forwarder: ack: %v", err)
		}
	}
}

// capsLoop pushes detected capabilities at startup and on a 60s ticker
// (spec §6.7 refresh): keeps last_seen fresh even without traffic.
func (a *App) capsLoop(ctx context.Context) {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.pushCapabilities(ctx); err != nil {
				log.Printf("foremanapp: capabilities push: %v", err)
			}
		}
	}
}

// pushCapabilities POSTs the detected capability tags plus the running
// build id (spec §6.7, §12.2) to the factory: POST
// /v1/foreman/capabilities with the paired bearer. A 200 body carries the
// factory's foreman_update advert (spec §12.3), handed to the updater; 204
// means nothing is advertised. A successful push is also the post-update
// confirm trigger (spec §12.5).
func (a *App) pushCapabilities(ctx context.Context) error {
	caps := detectCapabilities()
	caps["build"] = a.build
	status, body, err := postJSONResponse(ctx, a.factoryURL+"/v1/foreman/capabilities", a.paired.Bearer,
		map[string]any{"capabilities": caps, "active_jobs": a.liveJobIDs(), "claude_auth": a.claudeAuth.report(a.claudeTiers())})
	if err != nil {
		return err
	}
	adv, err := parseCapsResponse(status, body)
	if err != nil {
		log.Printf("foremanapp: auto-update: %v (ignored)", err)
	}
	a.observeClaudeDirective(ctx, parseClaudeDirective(status, body))
	a.updater.Observe(adv)
	a.updater.Confirm(ctx)
	return nil
}

// ackLocal acks ids in the local inbox service.
func (a *App) ackLocal(ctx context.Context, ids []string) error {
	return a.inbox.Ack(ctx, a.inboxID, ids)
}

// ackRemote acks ids in the factory-side copy of the foreman inbox:
// POST {FactoryURL}/v1/inboxes/{foreman:{id}}/ack with the paired bearer
// (spec §4.1; the notify.Client does not ack on its own).
func (a *App) ackRemote(ctx context.Context, ids []string) error {
	return postJSON(ctx,
		a.factoryURL+"/v1/inboxes/"+url.PathEscape(a.inboxID)+"/ack",
		a.paired.Bearer, map[string]any{"ids": ids})
}
