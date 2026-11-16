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

	"github.com/pricec/factory/internal/auth"
	"github.com/pricec/factory/internal/budgets"
	"github.com/pricec/factory/internal/inbox"
	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/mcpfactory"
	"github.com/pricec/factory/internal/models"
	"github.com/pricec/factory/internal/notify"
	"github.com/pricec/factory/internal/registry"
	"github.com/pricec/factory/internal/tasks"
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
	// Budget carries the spend-cap breaker config (spec §6.5).
	Budget budgets.Config
	// Now is the clock; nil -> real UTC time. Propagated to the jobs and
	// auth stores (sweeper, liveness, pairing TTLs all become
	// fake-clockable).
	Now func() time.Time
}

// App is the wired factory controller.
type App struct {
	jobs       *jobs.Store
	inbox      *inbox.Service
	tasks      *tasks.Store // shares inbox.db (spec §9.5.1); New runs the legacy backfill; inbox.Close closes it
	auth       *auth.Store
	authn      *auth.Authenticator
	hub        *notify.Hub
	sweeper    *jobs.Sweeper
	scheduler  *jobs.Scheduler
	picker     *jobs.RetryPicker
	pending    *jobs.PendingPicker
	matcher    *registry.CapabilityMatcher
	budget     budgets.Config
	planner    *PlannerBrain
	wakes      *wakeDispatcher // per-task planner wakes (spec §9.5.2, tasks_wake.go)
	reaper     *taskReaper     // idle-24h task auto-close (spec §9.5.2, tasks_reaper.go)
	handler    http.Handler
	advertise  string
	plannerTok string
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
	a.budget = cfg.Budget
	a.sweeper = jobs.NewSweeper(a.jobs)
	a.scheduler = jobs.NewScheduler(a.jobs, matcher)
	a.picker = jobs.NewRetryPicker(a.jobs)
	a.pending = jobs.NewPendingPicker(a.jobs, matcher)

	mcpH, err := mcpfactory.Handler(mcpfactory.Config{
		Auth:    a.auth,
		Store:   a.jobs,
		Matcher: matcher,
		Inbox:   a.inbox,
		Budget:  cfg.Budget,
		// Task tools + woken-task defaults (spec §9.5.2).
		Tasks:       a.tasks,
		TaskChanged: a.publishTaskChanged,
		// raise_alert tool (spec §9.5.3).
		RaiseAlert: a.RaisePlannerAlert,
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
	a.initTasks()
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
// factory:main consumer, the dispatch notifier, and the long-pending
// alert. It blocks until ctx is done.
func (a *App) Run(ctx context.Context) error {
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
func (a *App) onTerminal(jobID, _ string) {
	ctx := context.Background()
	if err := a.auth.InvalidateJob(ctx, jobID); err != nil {
		log.Printf("factoryapp: invalidate credentials for %s: %v", jobID, err)
	}
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
	mux.Handle("/v1/deadletter", a.jobs.DeadLetterHandler())
	mux.Handle("/v1/deadletter/", a.jobs.DeadLetterHandler())
	mux.Handle("/v1/spend", a.jobs.SpendHandler())
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
