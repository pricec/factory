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
	"github.com/pricec/factory/internal/notify"
	"github.com/pricec/factory/internal/registry"
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
	auth       *auth.Store
	authn      *auth.Authenticator
	hub        *notify.Hub
	sweeper    *jobs.Sweeper
	scheduler  *jobs.Scheduler
	picker     *jobs.RetryPicker
	handler    http.Handler
	advertise  string
	plannerTok string
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

	a := &App{advertise: cfg.AdvertiseURL}
	if a.advertise == "" {
		a.advertise = defaultAdvertiseURL(cfg.Addr)
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
	a.auth, err = auth.Open(filepath.Join(cfg.DataDir, "auth.db"))
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("factoryapp: open auth: %w", err)
	}
	a.jobs.Now = now
	a.auth.Now = now

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
	a.sweeper = jobs.NewSweeper(a.jobs)
	a.scheduler = jobs.NewScheduler(a.jobs, matcher)
	a.picker = jobs.NewRetryPicker(a.jobs)

	mcpH, err := mcpfactory.Handler(mcpfactory.Config{
		Auth:    a.auth,
		Store:   a.jobs,
		Matcher: matcher,
		Inbox:   a.inbox,
		Budget:  cfg.Budget,
	})
	if err != nil {
		_ = a.Close()
		return nil, fmt.Errorf("factoryapp: planner MCP: %w", err)
	}
	a.handler = buildMux(a, mcpH)
	return a, nil
}

// Handler returns the full /v1 mux.
func (a *App) Handler() http.Handler { return a.handler }

// PlannerToken returns the factory planner bearer (env-provided or minted).
func (a *App) PlannerToken() string { return a.plannerTok }

// AdvertiseURL returns the URL advertised to pairing children.
func (a *App) AdvertiseURL() string { return a.advertise }

// Jobs, Inbox, and Auth are integration/test accessors to the stores.
func (a *App) Jobs() *jobs.Store     { return a.jobs }
func (a *App) Inbox() *inbox.Service { return a.inbox }
func (a *App) Auth() *auth.Store     { return a.auth }

// Run runs the background loops: sweeper, scheduler, retry picker, the
// factory:main consumer, and the dispatch notifier. It blocks until ctx
// is done.
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
	run("consumer", func() error { a.consumeLoop(ctx); return nil })
	run("dispatch notifier", func() error { a.notifyLoop(ctx); return nil })
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
// worker tokens (spec §7.4) and flag over-budget jobs (spec §6.5 proxy 4).
func (a *App) onTerminal(jobID, _ string) {
	ctx := context.Background()
	if err := a.auth.InvalidateJob(ctx, jobID); err != nil {
		log.Printf("factoryapp: invalidate credentials for %s: %v", jobID, err)
	}
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
	mux.HandleFunc("POST /v1/pair", a.handlePair)
	mux.Handle("/v1/inboxes/", inbox.NewHandler(a.inbox, a.authn))
	mux.Handle("GET /v1/inboxes/{id}/stream", notify.StreamHandler(a.hub, a.authn))
	mux.Handle("/v1/foreman/capabilities", registry.RefreshHandler(a.refreshAuthz(), a.upsertCaps))
	mux.Handle("/v1/jobs", a.jobs.APIHandler())
	mux.Handle("/v1/jobs/", a.jobs.APIHandler())
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
