package auditor

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pricec/factory/internal/wire"
	"github.com/robfig/cron/v3"
)

// Hard guardrail ceilings (operator-approved, spec §6.12). Config can only
// LOWER these; raising them is a code change, and this package is on the
// auto-fix denylist, so the auditor can never raise them itself.
const (
	// HardMaxAutoFixes is the most auto-fixes one audit run may start.
	HardMaxAutoFixes = 5
	// HardMaxChangedLines caps one auto-fix PR (added + deleted lines).
	HardMaxChangedLines = 150
	// MaxJobPriority is the highest JobSpec priority (1-9, 9 highest) an
	// audit or fix job may carry: audits are LOW priority work.
	MaxJobPriority = 3
	// MinTaskPriority is the most urgent task priority the auditor may
	// file at. Task priority 0 is P0 and bypasses the usage throttle, so
	// the auditor never uses it.
	MinTaskPriority = 1
	// WorsenFactor: a finding the operator closed is re-filed only when
	// its metric is at least this many times the metric it was filed at.
	WorsenFactor = 2.0
)

// Identity and titles.
const (
	// CreatedBy marks the audit job and every fix job it spawns.
	CreatedBy = "factory:auditor"
	// ProposalPrefix titles every finding filed for operator approval.
	ProposalPrefix = "[proposal] "
	// AutoFixPrefix titles the informational task of an applied auto-fix.
	AutoFixPrefix = "[auto-fix] "
	// PayloadKindAudit / PayloadKindFix tag the job payloads.
	PayloadKindAudit = "factory_audit"
	PayloadKindFix   = "factory_audit_fix"
)

// Env names (factory process environment, e.g. the systemd unit).
const (
	EnvEnabled      = "FACTORY_AUDIT_ENABLED"
	EnvAutoFix      = "FACTORY_AUDIT_AUTOFIX"
	EnvCron         = "FACTORY_AUDIT_CRON"
	EnvTZ           = "FACTORY_AUDIT_TZ"
	EnvBudgetUSD    = "FACTORY_AUDIT_BUDGET_USD"
	EnvFixReserve   = "FACTORY_AUDIT_FIX_RESERVE_USD"
	EnvMaxAutoFixes = "FACTORY_AUDIT_MAX_AUTOFIXES"
	EnvMaxLines     = "FACTORY_AUDIT_MAX_CHANGED_LINES"
	EnvForeman      = "FACTORY_AUDIT_FOREMAN"
	EnvRepo         = "FACTORY_AUDIT_REPO"
	EnvRepoDir      = "FACTORY_AUDIT_REPO_DIR"
	EnvWorktreeRoot = "FACTORY_AUDIT_WORKTREE_ROOT"
)

// Config is the auditor's runtime configuration.
type Config struct {
	// Enabled installs/keeps the daily schedule; false removes it.
	Enabled bool
	// AutoFix lets a run act without asking (inside the guardrails);
	// false files every finding as a proposal.
	AutoFix bool
	// Cron is the 5-field schedule, interpreted in TZ (CronSpec).
	Cron string
	// TZ is the IANA zone the cron fires in ("" -> the host's zone).
	TZ string
	// BudgetUSD is the per-run cost_usd meter: a run stops starting new
	// work once the audit job plus its fix jobs reach it.
	BudgetUSD float64
	// FixReserveUSD is what a not-yet-finished fix job counts against the
	// meter (and its own max_cost_usd), so concurrent fixes cannot
	// overshoot the budget before their real cost is known.
	FixReserveUSD float64
	// MaxAutoFixes per run (<= HardMaxAutoFixes).
	MaxAutoFixes int
	// MaxChangedLines per auto-fix PR (<= HardMaxChangedLines).
	MaxChangedLines int
	// JobPriority of audit and fix jobs (1..MaxJobPriority).
	JobPriority int
	// TaskPriority of filed tasks (>= MinTaskPriority, never 0).
	TaskPriority int
	// SweepTier is the audit job's tier (cheapest: flash); FixTier the
	// fix jobs' (default).
	SweepTier, FixTier string
	// Foreman pins audit and fix jobs (the integration host that holds
	// the worktrees), full "foreman:{id}" form.
	Foreman string
	// Repo is the GitHub OWNER/REPO auto-fix PRs target.
	Repo string
	// RepoDir is the clone on the Foreman host whose worktrees the AUDIT
	// JOB scans (read-only) and cleanup jobs act on; "" tells the job to
	// skip the scan. The factory service itself never runs git.
	RepoDir string
	// WorktreeRoot is the only directory whose worktrees may be cleaned.
	WorktreeRoot string
	// StaleAfter is how old an unmerged worktree's last commit must be to
	// be reported as stale.
	StaleAfter time.Duration
}

// DefaultConfig is the operator-approved default (decisions recorded
// 2026-11-17): enabled and auto-fixing from the first run, daily 06:00
// local, $10 per run, at most 5 auto-fixes of at most 150 lines, flash
// sweep, default-tier fixes, job priority 1, task priority 3, cleanup only
// of merged clean worktrees under /home/price/work.
func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		AutoFix:         true,
		Cron:            "0 6 * * *",
		BudgetUSD:       10,
		FixReserveUSD:   1.5,
		MaxAutoFixes:    HardMaxAutoFixes,
		MaxChangedLines: HardMaxChangedLines,
		JobPriority:     1,
		TaskPriority:    3,
		SweepTier:       wire.TierFlash,
		FixTier:         wire.TierDefault,
		Foreman:         "foreman:server",
		Repo:            "pricec/factory",
		RepoDir:         "/home/price/temp/factory",
		WorktreeRoot:    "/home/price/work",
		StaleAfter:      7 * 24 * time.Hour,
	}
}

// LoadConfig applies env overrides to DefaultConfig and clamps every
// guardrail to its hard ceiling. Warnings name ignored or clamped values.
func LoadConfig(getenv func(string) string) (Config, []string) {
	if getenv == nil {
		getenv = os.Getenv
	}
	c := DefaultConfig()
	var warn []string
	boolEnv := func(name string, dst *bool) {
		switch strings.ToLower(strings.TrimSpace(getenv(name))) {
		case "":
		case "1", "true", "yes", "on":
			*dst = true
		case "0", "false", "no", "off":
			*dst = false
		default:
			warn = append(warn, name+": not a boolean, ignored")
		}
	}
	floatEnv := func(name string, dst *float64) {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || f <= 0 {
				warn = append(warn, name+": must be a positive number, ignored")
				return
			}
			*dst = f
		}
	}
	intEnv := func(name string, dst *int) {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 0 {
				warn = append(warn, name+": must be a non-negative integer, ignored")
				return
			}
			*dst = n
		}
	}
	strEnv := func(name string, dst *string) {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			*dst = v
		}
	}
	boolEnv(EnvEnabled, &c.Enabled)
	boolEnv(EnvAutoFix, &c.AutoFix)
	strEnv(EnvCron, &c.Cron)
	strEnv(EnvTZ, &c.TZ)
	floatEnv(EnvBudgetUSD, &c.BudgetUSD)
	floatEnv(EnvFixReserve, &c.FixReserveUSD)
	intEnv(EnvMaxAutoFixes, &c.MaxAutoFixes)
	intEnv(EnvMaxLines, &c.MaxChangedLines)
	strEnv(EnvForeman, &c.Foreman)
	strEnv(EnvRepo, &c.Repo)
	if v, ok := lookup(getenv, EnvRepoDir); ok {
		c.RepoDir = v // "" disables the git scan
	}
	strEnv(EnvWorktreeRoot, &c.WorktreeRoot)
	if !strings.HasPrefix(c.Foreman, "foreman:") {
		c.Foreman = "foreman:" + c.Foreman
	}
	if _, err := cron.ParseStandard(c.CronSpec()); err != nil {
		warn = append(warn, fmt.Sprintf("%s/%s: %v; using the default", EnvCron, EnvTZ, err))
		c.Cron, c.TZ = DefaultConfig().Cron, ""
	}
	warn = append(warn, c.clamp()...)
	return c, warn
}

// lookup distinguishes "set to empty" from unset for getenv funcs backed
// by os.LookupEnv semantics: an explicit "-" also means empty.
func lookup(getenv func(string) string, name string) (string, bool) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return "", false
	}
	if v == "-" {
		return "", true
	}
	return v, true
}

// clamp forces every guardrail inside its hard bounds.
func (c *Config) clamp() []string {
	var warn []string
	if c.MaxAutoFixes > HardMaxAutoFixes {
		warn = append(warn, fmt.Sprintf("max auto-fixes %d clamped to %d", c.MaxAutoFixes, HardMaxAutoFixes))
		c.MaxAutoFixes = HardMaxAutoFixes
	}
	if c.MaxChangedLines > HardMaxChangedLines || c.MaxChangedLines <= 0 {
		warn = append(warn, fmt.Sprintf("max changed lines %d clamped to %d", c.MaxChangedLines, HardMaxChangedLines))
		c.MaxChangedLines = HardMaxChangedLines
	}
	if c.JobPriority < wire.MinPriority || c.JobPriority > MaxJobPriority {
		c.JobPriority = 1
	}
	if c.TaskPriority < MinTaskPriority {
		c.TaskPriority = DefaultConfig().TaskPriority
	}
	if c.BudgetUSD <= 0 {
		c.BudgetUSD = DefaultConfig().BudgetUSD
	}
	if c.FixReserveUSD <= 0 {
		c.FixReserveUSD = DefaultConfig().FixReserveUSD
	}
	return warn
}

// CronSpec is the robfig cron spec the schedule stores: the 5-field Cron,
// prefixed with CRON_TZ so 06:00 means 06:00 on the factory host's clock
// (the scheduler's anchors are UTC).
func (c Config) CronSpec() string {
	tz := c.TZ
	if tz == "" {
		tz = LocalZone()
	}
	if tz == "" || strings.HasPrefix(c.Cron, "CRON_TZ=") || strings.HasPrefix(c.Cron, "TZ=") {
		return c.Cron
	}
	return "CRON_TZ=" + tz + " " + c.Cron
}

// LocalZone returns the host's IANA zone name: $TZ, then /etc/timezone,
// then the /etc/localtime symlink target; "" when unknown (cron in UTC).
func LocalZone() string {
	if tz := strings.TrimPrefix(os.Getenv("TZ"), ":"); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
	}
	if b, err := os.ReadFile("/etc/timezone"); err == nil {
		if tz := strings.TrimSpace(string(b)); tz != "" {
			if _, err := time.LoadLocation(tz); err == nil {
				return tz
			}
		}
	}
	if target, err := os.Readlink("/etc/localtime"); err == nil {
		if i := strings.Index(target, "zoneinfo/"); i >= 0 {
			tz := target[i+len("zoneinfo/"):]
			if _, err := time.LoadLocation(tz); err == nil {
				return tz
			}
		}
	}
	return ""
}
