// Package factoryapp wires the factory controller binary (issue #20): the
// inbox, auth, jobs, notify, registry, budgets, and planner MCP packages
// composed into one HTTP mux plus the background loops (sweeper,
// scheduler, retry picker, factory:main consumer, dispatch notifier).
package factoryapp

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pricec/factory/internal/usagewin"

	"github.com/pricec/factory/internal/claudecli"

	"github.com/pricec/factory/internal/auditor"
	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/budgets"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/mcpfactory"
	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/notify"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/secrets"
	"github.com/pricec/factory/internal/tasks"
	"github.com/pricec/factory/internal/throttle"
	"github.com/pricec/factory/internal/wire"
)

// FactoryMainInbox is the factory's processing inbox: foreman-originated
// job.event / forwarded job.result land here (spec §7.3).
const FactoryMainInbox = "factory:main"

// Config configures the factory App.
type Config struct {
	// DataDir holds the three sqlite files: jobs.db, inbox.db, auth.db.
	DataDir string
	// Addr is the HTTP listen address, e.g. ":8080". It is used by
	// cmd/factory's real listener; App only reads its port for the
	// computed advertise URL.
	Addr string
	// AdvertiseURL is the base URL advertised to pairing children; empty
	// -> http://<hostname>:<port of Addr>.
	AdvertiseURL string
	// DownloadsDir serves GET /download/foreman/{platform} (#24): the
	// pre-built foreman binaries (see the Makefile release target). Empty
	// -> ./downloads, resolved in New.
	DownloadsDir string
	// Now is the clock; nil -> real UTC time. Propagated to the jobs and
	// auth stores (sweeper, liveness, pairing TTLs all become
	// fake-clockable).
	Now func() time.Time
	// Usage configures the S2 usage-window tracker (observe-only).
	Usage UsageConfig
	// Throttle configures the S3 dispatch gate (throttle.go).
	Throttle ThrottleConfig
	// MergeForeman is the integration foreman merge jobs are pinned to
	// (spec §6.11, mergequeue.go); empty -> FACTORY_MERGE_FOREMAN, then
	// foreman:server.
	MergeForeman string
	// Audit configures the daily factory auditor (spec §6.12, auditor.go);
	// nil disables it (cmd/factory passes auditor.LoadConfig of the
	// FACTORY_AUDIT_* env).
	Audit *auditor.Config
}

// App is the wired factory controller.
type App struct {
	jobs      *jobs.Store
	inbox     *inbox.Service
	tasks     *tasks.Store // shares inbox.db (spec §9.5.1); New runs the legacy backfill; inbox.Close closes it
	auth      *auth.Store
	secrets   *secrets.Store // credentials store (spec §13)
	authn     *auth.Authenticator
	hub       *notify.Hub
	sweeper   *jobs.Sweeper
	scheduler *jobs.Scheduler
	picker    *jobs.RetryPicker
	pending   *jobs.PendingPicker
	matcher   *registry.CapabilityMatcher
	planner   *PlannerBrain
	wakes     *wakeDispatcher // per-task planner wakes (spec §9.5.2, tasks_wake.go)
	reaper    *taskReaper     // idle-24h task auto-close (spec §9.5.2, tasks_reaper.go)
	handler   http.Handler
	usage     *usagewin.Tracker
	usagePoll *usagewin.Poller // nil unless Usage.Poll
	gate      *throttle.Gate   // S3 dispatch gate (throttle.go)
	// S5 pause (pause.go): jobs that hit the limit (job id -> true), and
	// test seams for the re-probe, jitter and wake poll interval.
	limitHit    sync.Map
	probe       func(ctx context.Context) error
	pauseJitter func() time.Duration
	wakePoll    time.Duration
	advertise   string
	plannerTok  string

	selfCancels sync.Map // job ids the planner is cancelling itself (planner.go)

	// downloadsDir backs GET /download/foreman/{platform} (#24).
	downloadsDir string

	// pendingAlertedJobs dedupes the long-pending alert (spec §6.7,
	// 2026-11-15 amendment): one notify.pending per job per pending
	// episode, keyed by the job row's pending-since stamp (updated_at) so
	// a requeue re-alerts; process-local and at-least-once like the
	// dispatch notifier's notified set. Guarded by pendingAlertMu (the
	// loop and test-driven passes may interleave).
	pendingAlertedJobs map[string]string
	pendingAlertMu     sync.Mutex

	// Merge queue (spec §6.11, mergequeue.go): mergeMu serializes passes,
	// mergeKick wakes the loop, mergeForeman is the integration pin.
	mergeMu      sync.Mutex
	mergeKick    chan struct{}
	mergeForeman string

	// audit is the factory auditor (spec §6.12, auditor.go).
	audit *auditRuntime
}

// New opens the three sqlite stores under cfg.DataDir, wires every
// component together, and builds the /v1 mux. It starts no goroutines and
// no listeners; Run owns the background loops.
func New(cfg Config) (*App, error) {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, fmt.Errorf("factoryapp: data dir: %w", err)
	}
	now := cfg.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	a := &App{
		advertise:          cfg.AdvertiseURL,
		pendingAlertedJobs: map[string]string{},
		mergeKick:          make(chan struct{}, 1),
		mergeForeman:       mergeForemanFrom(cfg.MergeForeman, os.Getenv(EnvMergeForeman)),
	}
	if a.advertise == "" {
		a.advertise = defaultAdvertiseURL(cfg.Addr)
	}
	a.downloadsDir = cfg.DownloadsDir
	if a.downloadsDir == "" {
		a.downloadsDir = "./downloads" // default -downloads dir (cmd/factory)
	}

	// Stores: open all three or none.
	var err error
	a.jobs, err = jobs.Open(filepath.Join(cfg.DataDir, "jobs.db"))
	if err != nil {
		return nil, fmt.Errorf("factoryapp: open jobs: %w", err)
	}
	a.inbox, err = inbox.Open(filepath.Join(cfg.DataDir, "inbox.db"))
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("factoryapp: open inbox: %w", err)
	}
	a.tasks, err = tasks.New(context.Background(), a.inbox.DB())
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("factoryapp: open tasks: %w", err)
	}
	a.auth, err = auth.Open(filepath.Join(cfg.DataDir, "auth.db"))
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("factoryapp: open auth: %w", err)
	}
	a.secrets, err = secrets.Open(filepath.Join(cfg.DataDir, secrets.FileName))
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("factoryapp: open secrets: %w", err)
	}
	a.secrets.Now = now
	a.jobs.Now = now
	a.auth.Now = now
	a.tasks.Now = now

	// Liveness: any authed request or SSE connect bumps last_seen (spec
	// §6.3); the SSE hub reports connect/disconnect.
	liveness := jobs.NewLiveness(a.jobs)
	a.authn = auth.NewAuthenticator(a.auth)
	a.authn.SetActivityHook(func(identity string) { liveness.OnActivity(identity, now()) })

	// Terminal hook (spec §6.5 proxy 4, §7.4): revoke the job's worker
	// credentials, then flag over-budget jobs post-hoc.
	a.jobs.SetOnTerminal(a.onTerminal)

	// Planner credential: pre-arranged env bearer or freshly minted.
	ctx := context.Background()
	if envTok := os.Getenv(auth.EnvPlannerToken); envTok != "" {
		if err := a.auth.RegisterPlannerBearer(ctx, envTok); err != nil {
			_ = a.Close()
			return nil, fmt.Errorf("factoryapp: register planner bearer: %w", err)
		}
		a.plannerTok = envTok
	} else {
		tok, err := a.auth.MintPlannerToken(ctx, FactoryMainInbox)
		if err != nil {
			_ = a.Close()
			return nil, fmt.Errorf("factoryapp: mint planner token: %w", err)
		}
		a.plannerTok = tok
		log.Printf("factoryapp: planner token: %s", tok)
		log.Printf("factoryapp: planner token usage: Authorization: Bearer <token> on /v1/mcp/factory")
	}

	// SSE fan-out: every Post pushes a wake-up to live streams.
	a.hub = notify.NewHub()
	a.hub.SetObserver(liveness)
	a.hub.AttachTo(a.inbox)

	matcher := &registry.CapabilityMatcher{Source: &foremanSource{store: a.jobs}}
	a.matcher = matcher
	a.sweeper = jobs.NewSweeper(a.jobs)
	a.scheduler = jobs.NewScheduler(a.jobs, matcher)
	a.picker = jobs.NewRetryPicker(a.jobs)
	a.pending = jobs.NewPendingPicker(a.jobs, matcher)

	mcpH, err := mcpfactory.Handler(mcpfactory.Config{
		Auth:    a.auth,
		Store:   a.jobs,
		Matcher: matcher,
		Inbox:   a.inbox,
		Secrets: a.secrets, // create_job secrets validation (spec §13.2)
		// Task tools + woken-task defaults (spec §9.5.2).
		Tasks:       a.tasks,
		TaskChanged: a.publishTaskChanged,
		// raise_alert tool (spec §9.5.3).
		RaiseAlert: a.RaisePlannerAlert,
		// cancel_job marks its own cancels so they do not wake the planner.
		MarkSelfCancel: a.markSelfCancel,
		// request_merge kicks the merge queue (spec §6.11).
		MergeQueued: a.kickMergeQueue,
	})
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("factoryapp: planner MCP: %w", err)
	}

	// Conversational planner (spec §9.5): one-shot CLI sessions over
	// the planner MCP surface, woken by the planner loop on chat.message
	// envelopes in the operator inbox. The runner routes on the session
	// model (spec §9.4, the §8.4 twin): claude-* models run the Claude
	// Code CLI, everything else opencode. Model: FACTORY_PLANNER_MODEL
	// overrides the pinned claude default (spec §6.5). The MCP URL
	// is loopback, not the advertised URL: the child runs on this very
	// host, and the MCP SDK's streamable-HTTP server rejects non-loopback
	// Host headers (DNS-rebinding protection) - the advertised hostname
	// gets "403 Forbidden: invalid Host header" and the child silently
	// runs without any factory tools.
	a.planner = &PlannerBrain{
		Model:   models.Resolve(os.Getenv("FACTORY_PLANNER_MODEL"), models.PlannerDefault),
		MCPURL:  loopbackMCPURL(cfg.Addr) + "/v1/mcp/factory",
		Bearer:  a.plannerTok,
		WorkDir: filepath.Join(cfg.DataDir, "planner"),
	}
	a.planner.OnUsage = func(ctx context.Context, taskID string, u claudecli.Usage) {
		ev := wire.UsageEvent{
			Kind: wire.UsageKindPlanner, TaskID: taskID, Model: u.Model, Input: u.Input, Output: u.Output,
			CacheCreate: u.CacheCreate, CacheRead: u.CacheRead, CostUSD: u.CostUSD, SessionID: u.SessionID, Limit: u.Limit, RateLimit: u.RateLimit,
		}
		if ev.Model == "" {
			ev.Model = a.planner.Model
		}
		if err := a.recordUsage(ctx, ev); err != nil {
			log.Printf("factoryapp: record planner usage: %v", err)
		}
	}
	a.initUsage(cfg)
	a.initTasks()
	a.initThrottle(cfg)
	a.initAuditor(cfg)
	a.handler = buildMux(a, mcpH)
	return a, nil
}

// Handler returns the full /v1 mux.
func (a *App) Handler() http.Handler { return a.handler }

// PlannerToken returns the factory planner bearer (env-provided or minted).
func (a *App) PlannerToken() string { return a.plannerTok }

// SetPlannerExec is a test seam: it replaces the planner brain's session
// runner so no test ever invokes a real child CLI (the BrainRunner.Exec
// pattern of foremanapp, #9.3).
func (a *App) SetPlannerExec(fn func(ctx context.Context, prompt string) (string, error)) {
	a.planner.Exec = fn
}

// AdvertiseURL returns the URL advertised to pairing children.
func (a *App) AdvertiseURL() string { return a.advertise }

// Jobs, Inbox, and Auth are integration/test accessors to the stores.
func (a *App) Jobs() *jobs.Store     { return a.jobs }
func (a *App) Inbox() *inbox.Service { return a.inbox }
func (a *App) Auth() *auth.Store     { return a.auth }

// Run runs the background loops: sweeper, scheduler, retry picker, the
// factory:main consumer, the dispatch notifier, the long-pending alert,
// and the merge queue. It blocks until ctx is done.
func (a *App) Run(ctx context.Context) error {
	if err := a.EnsureAuditSchedule(ctx); err != nil {
		log.Printf("factoryapp: auditor: schedule: %v", err)
	}
	var wg sync.WaitGroup
	run := func(name string, fn func() error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := fn(); err != nil && ctx.Err() == nil {
				log.Printf("factoryapp: %s stopped: %v", name, err)
			}
		}()
	}
	run("sweeper", func() error { return a.sweeper.Run(ctx) })
	run("scheduler", func() error { return a.scheduler.Run(ctx) })
	run("retry picker", func() error { return a.picker.Run(ctx) })
	run("pending picker", func() error { return a.pending.Run(ctx) })
	run("consumer", func() error { a.consumeLoop(ctx); return nil })
	run("planner", func() error { a.plannerLoop(ctx); return nil })
	run("dispatch notifier", func() error { a.notifyLoop(ctx); return nil })
	run("alert expiry", func() error { a.alertExpiryLoop(ctx); return nil })
	run("pending alert", func() error { a.pendingAlertLoop(ctx); return nil })
	run("merge queue", func() error { a.mergeQueueLoop(ctx); return nil })
	if a.usagePoll != nil {
		run("usage poller", func() error { a.usagePoll.Run(ctx); return nil })
	}
	run("throttle releaser", func() error { a.releaserLoop(ctx); return nil })
	run("usage pause", func() error { a.pauseLoop(ctx); return nil })
	run("task reaper", func() error { a.reaper.Run(ctx, taskReapInterval); return nil })
	wg.Wait()
	return nil
}

// Close closes the three stores.
func (a *App) Close() error {
	var errs []error
	if a.inbox != nil {
		errs = append(errs, a.inbox.Close())
	}
	if a.jobs != nil {
		errs = append(errs, a.jobs.Close())
	}
	if a.auth != nil {
		errs = append(errs, a.auth.Close())
	}
	return errors.Join(errs...)
}

// onTerminal fires on every terminal job transition: revoke the job's
// worker tokens (spec §7.4), flag over-budget jobs (spec §6.5 proxy 4),
// and bring the terminal state to the operator's attention (spec §9.5).
func (a *App) onTerminal(jobID, finalState string) {
	ctx := context.Background()
	if err := a.auth.InvalidateJob(ctx, jobID); err != nil {
		log.Printf("factoryapp: invalidate credentials for %s: %v", jobID, err)
	}
	// A merge job's terminal transition closes its merge request and
	// releases the queue (spec §6.11).
	a.finishMerge(ctx, jobID, finalState)
	// Audit runs and their fix jobs (spec §6.12, auditor.go).
	a.auditTerminal(ctx, jobID, finalState)
	a.notifyOperatorJob(ctx, jobID)
	job, ok, err := a.jobs.Get(ctx, jobID)
	if err != nil || !ok {
		return
	}
	var usage wire.Usage
	if len(job.Usage) > 0 && string(job.Usage) != "null" {
		if err := jsonUnmarshal(job.Usage, &usage); err != nil {
			log.Printf("factoryapp: usage of %s: %v", jobID, err)
			return
		}
	}
	if budgets.ShouldFlagOverBudget(usage, job.Spec.Budget.MaxCostUSD) {
		if err := a.jobs.MarkOverBudget(ctx, jobID); err != nil {
			log.Printf("factoryapp: mark %s over budget: %v", jobID, err)
		}
	}
}

// buildMux mounts every /v1 surface on a single mux.
func buildMux(a *App, mcpH http.Handler) http.Handler {
	mux := http.NewServeMux()
	// Operator UI (spec §2): GET / serves the page; the two /v1/inboxes UI
	// endpoints below are registered before the authed inbox subtree so the
	// more specific patterns take precedence.
	mux.HandleFunc("GET /{$}", a.handleUI)
	mux.HandleFunc("GET /manifest.webmanifest", a.handleUIAsset)
	mux.HandleFunc("GET /apple-touch-icon.png", a.handleUIAsset)
	mux.HandleFunc("GET /icon-512.png", a.handleUIAsset)
	mux.HandleFunc("GET /v1/inboxes", a.handleInboxSummaries)
	mux.HandleFunc("GET /v1/inboxes/{id}/recent", a.handleInboxRecent)
	// Conversational surface (spec §9.5): the UI's send path into the
	// operator inbox; the planner loop wakes the LLM on chat.message.
	// The permissive planner:main SSE stream pushes new envelopes to the
	// browser chat tab (no bearer: the UI surface is unauthenticated,
	// §7.5); the literal pattern outranks the authed {id} wildcard.
	mux.HandleFunc("POST /v1/chat", a.handleChat)
	a.registerTaskRoutes(mux) // spec §9.5.2: /v1/tasks (tasks_http.go)
	mux.Handle("GET /v1/inboxes/planner:main/stream", notify.StreamHandler(a.hub, nil))
	// Brain drive surface (spec §6.9): UI twins of the MCP create_job and
	// message_foreman tools (§9.4) - same stores, same matcher, same breaker.
	a.mountAlertRoutes(mux)
	mux.HandleFunc("POST /v1/jobs", a.handleCreateJob)
	mux.HandleFunc("POST /v1/foremen/{id}/messages", a.handleMessageForeman)
	// Onboarding surfaces (#24): top-level, outside /v1. /install.sh is
	// planner-authed; downloads are unauthenticated per spec §7.5.
	mux.HandleFunc("GET /install.sh", a.handleInstall)
	mux.HandleFunc("GET /install-server.sh", a.handleInstallServer)
	mux.HandleFunc("GET /download/factory/{platform}", a.handleDownload)
	mux.HandleFunc("GET /download/foreman/{platform}", a.handleDownload)
	mux.HandleFunc("POST /v1/pair", a.handlePair)
	mux.HandleFunc("POST /v1/pairing-tokens", a.handlePairingTokens)
	mux.Handle("/v1/inboxes/", inbox.NewHandler(a.inbox, a.authn))
	mux.Handle("GET /v1/inboxes/{id}/stream", notify.StreamHandler(a.hub, a.authn))
	mux.Handle("/v1/foreman/capabilities", registry.RefreshHandlerWithAdvert(a.refreshAuthz(), a.upsertCaps, registry.NewAdvertiser(a.downloadsDir).Advert))
	mux.Handle("/v1/jobs", a.jobs.APIHandler())
	mux.Handle("/v1/jobs/", a.jobs.APIHandler())
	mux.HandleFunc("GET /v1/jobs/{id}", a.handleGetJob) // + task_id (§9.5.2)
	mux.Handle("/v1/foremen", a.jobs.APIHandler())
	mux.Handle("/v1/schedules", a.jobs.SchedulesHandler())
	mux.Handle("/v1/schedules/", a.jobs.SchedulesHandler())
	// Merge queue (spec §6.11): every mutation kicks the dispatch loop.
	mergeH := a.jobs.MergeRequestsHandler(a.kickMergeQueue)
	mux.Handle("/v1/merge-requests", mergeH)
	mux.Handle("/v1/merge-requests/", mergeH)
	mux.Handle("/v1/deadletter", a.jobs.DeadLetterHandler())
	mux.Handle("/v1/deadletter/", a.jobs.DeadLetterHandler())
	mux.Handle("/v1/spend", a.jobs.SpendHandler())
	mux.HandleFunc("GET /v1/secrets", a.handleListSecrets)            // credentials (spec §13): metadata only
	mux.HandleFunc("POST /v1/secrets/{name}", a.handlePutSecret)      // upsert; never echoes the value
	mux.HandleFunc("DELETE /v1/secrets/{name}", a.handleDeleteSecret) // no value-returning route exists
	mux.HandleFunc("GET /v1/jobs/{id}/secrets", a.handleJobSecrets)   // job handout (spec §13.2): the only value-returning route
	mux.HandleFunc("GET /v1/usage", a.handleUsage)                    // S2: window state (usage.go)
	mux.HandleFunc("GET /v1/throttle", a.handleGetThrottle)           // S3: gate (throttle.go)
	mux.HandleFunc("POST /v1/throttle", a.handlePostThrottle)         // S3: operator override
	mux.Handle("/v1/mcp/factory", mcpH)
	return mux
}

// defaultAdvertiseURL computes http://<hostname>:<port of Addr>.
func defaultAdvertiseURL(addr string) string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "localhost"
	}
	port := "8080"
	if i := strings.LastIndex(addr, ":"); i >= 0 && addr[i+1:] != "" {
		port = addr[i+1:]
	}
	return "http://" + net.JoinHostPort(host, port)
}

// loopbackMCPURL returns the loopback base URL for the planner child's
// MCP endpoint (the port of Addr, default 8080). The MCP SDK's
// streamable-HTTP server validates Host headers against loopback
// (DNS-rebinding protection): the advertised hostname is rejected with
// "403 Forbidden: invalid Host header", which silently strips the child
// of every factory tool.
func loopbackMCPURL(addr string) string {
	port := "8080"
	if i := strings.LastIndex(addr, ":"); i >= 0 && addr[i+1:] != "" {
		port = addr[i+1:]
	}
	return "http://127.0.0.1:" + port
}
