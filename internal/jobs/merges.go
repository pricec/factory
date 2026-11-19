// Merge queue (2026-11-17 amendment, spec §6.11): merge_requests rows are
// the factory-tracked replacement for ad-hoc "merge and deploy" prose
// jobs. A request is queued, the factory's merge-queue loop (factoryapp)
// dispatches at most ONE at a time as a merge job pinned to the
// integration foreman, and the job's terminal transition closes the
// request. This file owns the persisted resource, its state machine, the
// persisted pause flag, and the /v1/merge-requests HTTP surface; the
// serialization loop and the operator alert live in factoryapp.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/oklog/ulid/v2"
)

// Merge request states (spec §6.11).
const (
	MergeQueued         = "queued"
	MergeMerging        = "merging"
	MergeLanded         = "landed"
	MergeNeedsAttention = "needs_attention"
	MergeCancelled      = "cancelled"
)

// MergeStates is every merge request state, for validation and tests.
var MergeStates = []string{MergeQueued, MergeMerging, MergeLanded, MergeNeedsAttention, MergeCancelled}

// mergeLegal is the normative transition table (spec §6.11): queued ->
// merging|cancelled; merging -> landed|needs_attention|cancelled; landed,
// needs_attention and cancelled are terminal.
var mergeLegal = map[string]map[string]bool{
	MergeQueued:  {MergeMerging: true, MergeCancelled: true},
	MergeMerging: {MergeLanded: true, MergeNeedsAttention: true, MergeCancelled: true},
}

// MergeTransitionLegal reports whether from -> to is a legal merge request
// transition.
func MergeTransitionLegal(from, to string) bool { return mergeLegal[from][to] }

// MergeTerminal reports whether a merge request state admits no further
// transitions.
func MergeTerminal(state string) bool {
	return state == MergeLanded || state == MergeNeedsAttention || state == MergeCancelled
}

// MergeStateForJob maps a merge job's terminal job state to the request's
// terminal state (spec §6.11): done -> landed only when the result has
// merged:true (FinishMergeForJob; otherwise needs_attention); failed|timed_out|dead ->
// needs_attention; cancelled -> cancelled. superseded (an operator replaced
// the merge job by hand) also needs attention: the queue cannot know
// whether the replacement merged. ok is false for non-terminal states
// other than failed (failed is only reached here when a merge job is
// dead-lettered without retry, see consumer).
func MergeStateForJob(jobState string) (state string, ok bool) {
	switch jobState {
	case StateDone:
		return MergeLanded, true
	case StateFailed, StateTimedOut, StateDead, StateSuperseded:
		return MergeNeedsAttention, true
	case StateCancelled:
		return MergeCancelled, true
	}
	return "", false
}

// DefaultMergeRepo is the repo a merge request targets when none is given
// (the gh OWNER/REPO form).
const DefaultMergeRepo = "pricec/factory"

// DefaultMergeBranch is the integration branch of DefaultMergeRepo.
const DefaultMergeBranch = "master"

// MergeBaseBranch is the default (integration) branch a merge request for
// repo targets: master for the factory repo, main for every other repo
// (e.g. video-chat). The merge job still reads the real default branch
// from GitHub (docs/merging.md step 2); this is the value the request and
// its task text state up front. repo may be a bare name.
func MergeBaseBranch(repo string) string {
	r := NormalizeRepo(repo)
	if r == "" || r == DefaultMergeRepo {
		return DefaultMergeBranch
	}
	return "main"
}

// Merge request field bounds (spec §6.11).
const (
	maxMergeNoteBytes   = 2000
	maxMergeBranchBytes = 200
	maxMergeResultBytes = 1000
)

var (
	// branchRe admits git branch names made of safe characters only: the
	// value is interpolated into a worker prompt and shell commands, so
	// anything else (spaces, quotes, $, ;, ..) is rejected outright.
	branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)
	// headRe is an abbreviated (7+) or full (40) lowercase hex commit sha.
	headRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)
	// repoRe is the gh OWNER/REPO form.
	repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
)

// Merge request errors (handlers map them to the §10 model).
var (
	ErrMergeNotFound  = errors.New("jobs: no such merge request")
	ErrMergeTerminal  = errors.New("jobs: merge request is already terminal")
	ErrMergeDuplicate = errors.New("jobs: an active merge request for this branch already exists")
)

// RequestedByJob is requested_by for a request a job filed for its own
// branch (job-owned merges, spec §6.11); source_job_id names the job.
const RequestedByJob = "job"

// maxMergeAfter caps the after list (spec §6.11 dependency gate).
const maxMergeAfter = 8

// mergeColumns are the job-owned-merge columns added after the first
// release of the table (addColumnIfMissing, see migrate).
var mergeColumns = [][2]string{
	{"source_job_id", "TEXT NOT NULL DEFAULT ''"},
	{"after_branches", "TEXT NOT NULL DEFAULT ''"},
	{"fix_round", "INTEGER NOT NULL DEFAULT 0"},
	{"failure", "TEXT NOT NULL DEFAULT ''"},
}

// errBadMerge marks 400-level merge request failures.
type errBadMerge struct{ msg string }

func (e *errBadMerge) Error() string { return e.msg }

// IsBadMergeRequest reports whether err is a 400-level validation failure
// from CreateMergeRequest (the MCP tool surfaces it as a tool error).
func IsBadMergeRequest(err error) bool {
	var bad *errBadMerge
	return errors.As(err, &bad)
}

const mergeDDL = `
CREATE TABLE IF NOT EXISTS merge_requests (
	id           TEXT PRIMARY KEY,
	repo         TEXT NOT NULL,
	branch       TEXT NOT NULL,
	head         TEXT NOT NULL,
	note         TEXT NOT NULL DEFAULT '',
	requested_by TEXT NOT NULL DEFAULT '',
	job_id       TEXT NOT NULL DEFAULT '',
	state        TEXT NOT NULL,
	result       TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL,
	updated_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_merge_requests_state ON merge_requests(state);
CREATE INDEX IF NOT EXISTS idx_merge_requests_job ON merge_requests(job_id);
-- Serialization backstop (spec §6.11): at most one row may be merging.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_merge_requests_merging ON merge_requests(state) WHERE state = 'merging';
CREATE TABLE IF NOT EXISTS merge_queue (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	paused     INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL DEFAULT ''
);
INSERT OR IGNORE INTO merge_queue (id, paused, updated_at) VALUES (1, 0, '');
`

// MergeRequest is one merge_requests row (spec §6.11).
type MergeRequest struct {
	ID          string `json:"id"`
	Repo        string `json:"repo"`
	Branch      string `json:"branch"`
	Head        string `json:"head"`
	Note        string `json:"note"`
	RequestedBy string `json:"requested_by"`
	// JobID is the merge job, set when the request starts merging.
	JobID string `json:"job_id,omitempty"`
	State string `json:"state"`
	// Result summarizes the outcome; set on the terminal transition.
	Result string `json:"result,omitempty"`
	// SourceJobID is the job that filed the request for its own branch
	// (job-owned merges, spec §6.11); "" for operator/planner/auditor
	// requests. The outcome is tracked against this job's task.
	SourceJobID string `json:"source_job_id,omitempty"`
	// After lists branches (same repo) that must have a landed merge
	// request before this one dispatches (the dependency gate).
	After []string `json:"after,omitempty"`
	// WaitingOn is the subset of After not landed yet; computed on read
	// for queued requests, never stored.
	WaitingOn []string `json:"waiting_on,omitempty"`
	// FixRound counts the automatic fix rounds behind this request: 0 for
	// a first request, n when the requesting job is the n-th merge-fix
	// job of the chain (bounded by MaxMergeFixRounds).
	FixRound int `json:"fix_round,omitempty"`
	// Failure is the classified failure kind of a needs_attention request
	// (MergeFailure* constants); "" otherwise.
	Failure   string `json:"failure,omitempty"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// MergeQueue is the GET /v1/merge-requests view: the persisted pause flag
// plus the requests, oldest first.
type MergeQueue struct {
	Paused bool `json:"paused"`
	// PausedChangedAt is when the pause flag last changed ("" = never).
	PausedChangedAt string         `json:"paused_changed_at,omitempty"`
	Requests        []MergeRequest `json:"requests"`
}

// NewMergeRequest is the create input.
type NewMergeRequest struct {
	Repo        string `json:"repo"`
	Branch      string `json:"branch"`
	Head        string `json:"head"`
	Note        string `json:"note"`
	RequestedBy string `json:"-"`
	// JobID is the requesting job for a job-owned merge ("" otherwise);
	// stored as source_job_id.
	JobID string `json:"job_id"`
	// After: branches that must land first (the dependency gate).
	After []string `json:"after"`
}

func newMergeRequestID() string { return "mr_" + ulid.Make().String() }

// validate normalizes (trim, default repo, lowercase head) and checks in.
func (in *NewMergeRequest) validate() error {
	in.JobID = strings.TrimSpace(in.JobID)
	if len(in.JobID) > 100 || strings.ContainsAny(in.JobID, " \t\r\n\"'`$;") {
		return &errBadMerge{"job_id: not a job id"}
	}
	in.Repo = strings.TrimSpace(in.Repo)
	in.Branch = strings.TrimSpace(in.Branch)
	in.Head = strings.ToLower(strings.TrimSpace(in.Head))
	in.Note = strings.TrimSpace(in.Note)
	if in.Repo == "" {
		in.Repo = DefaultMergeRepo
	} else if !strings.Contains(in.Repo, "/") && repoRe.MatchString("x/"+in.Repo) {
		in.Repo = defaultRepoOwner + "/" + in.Repo // bare name, e.g. "video-chat"
	}
	switch {
	case in.Branch == "":
		return &errBadMerge{"branch: required"}
	case len(in.Branch) > maxMergeBranchBytes:
		return &errBadMerge{fmt.Sprintf("branch: max %d bytes", maxMergeBranchBytes)}
	case !validBranchName(in.Branch):
		return &errBadMerge{fmt.Sprintf("branch: %q is not an acceptable branch name ([A-Za-z0-9._/-], no ..)", in.Branch)}
	case in.Branch == "master" || in.Branch == "main" || in.Branch == "HEAD":
		return &errBadMerge{fmt.Sprintf("branch: %q is the integration branch itself", in.Branch)}
	case in.Head == "":
		return &errBadMerge{"head: required (the commit sha the branch must point at)"}
	case !headRe.MatchString(in.Head):
		return &errBadMerge{fmt.Sprintf("head: %q must be a 7-40 char hex commit sha", in.Head)}
	case !repoRe.MatchString(in.Repo):
		return &errBadMerge{fmt.Sprintf("repo: %q must be OWNER/REPO", in.Repo)}
	case len(in.Note) > maxMergeNoteBytes:
		return &errBadMerge{fmt.Sprintf("note: max %d bytes", maxMergeNoteBytes)}
	case !utf8.ValidString(in.Note):
		return &errBadMerge{"note: must be valid UTF-8"}
	}
	var after []string
	for _, dep := range in.After {
		dep = strings.TrimSpace(dep)
		switch {
		case dep == "":
			continue
		case len(dep) > maxMergeBranchBytes || !validBranchName(dep):
			return &errBadMerge{fmt.Sprintf("after: %q is not an acceptable branch name", dep)}
		case dep == in.Branch:
			return &errBadMerge{fmt.Sprintf("after: %q is the branch being merged", dep)}
		}
		if !containsString(after, dep) {
			after = append(after, dep)
		}
	}
	if len(after) > maxMergeAfter {
		return &errBadMerge{fmt.Sprintf("after: max %d branches", maxMergeAfter)}
	}
	in.After = after
	return nil
}

// validBranchName is the §6.11 branch rule shared by branch and after.
func validBranchName(b string) bool {
	return branchRe.MatchString(b) && !strings.Contains(b, "..") &&
		!strings.HasSuffix(b, "/") && !strings.HasSuffix(b, ".lock")
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// CreateMergeRequest validates in and stores it queued (spec §6.11). A
// second active (queued or merging) request for the same repo+branch is
// ErrMergeDuplicate: cancel the first to re-request at a new head. One
// exception keeps job-owned work moving: a job (JobID set) re-requesting
// its branch while the earlier request is still only queued replaces it
// (the old one is cancelled, "superseded by ..."), so a job that pushed
// one more commit is never stuck behind its own stale request. A merging
// request is never replaced.
//
// A request filed by a merge-fix job (MergeFixCreatedBy) inherits that
// job's fix round, which is what bounds the automatic fix loop.
func (s *Store) CreateMergeRequest(ctx context.Context, in NewMergeRequest) (MergeRequest, error) {
	if err := in.validate(); err != nil {
		return MergeRequest{}, err
	}
	var dup, dupState string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, state FROM merge_requests WHERE repo = ? AND branch = ? AND state IN (?, ?) LIMIT 1`,
		in.Repo, in.Branch, MergeQueued, MergeMerging).Scan(&dup, &dupState)
	switch {
	case err == nil:
		if in.JobID == "" || dupState != MergeQueued {
			return MergeRequest{}, fmt.Errorf("%w: %s is %s (%s)", ErrMergeDuplicate, dup, dupState, in.Branch)
		}
		if _, applied, err := s.TransitionMerge(ctx, dup, MergeCancelled,
			"superseded by a newer request for "+in.Branch+" @ "+in.Head+" from job "+in.JobID); err != nil {
			return MergeRequest{}, err
		} else if !applied {
			return MergeRequest{}, fmt.Errorf("%w: %s (%s)", ErrMergeDuplicate, dup, in.Branch)
		}
	case !errors.Is(err, sql.ErrNoRows):
		return MergeRequest{}, err
	}
	now := s.now()
	mr := MergeRequest{
		ID: newMergeRequestID(), Repo: in.Repo, Branch: in.Branch, Head: in.Head, Note: in.Note,
		RequestedBy: in.RequestedBy, State: MergeQueued, CreatedAt: now, UpdatedAt: now,
		SourceJobID: in.JobID, After: in.After,
	}
	if in.JobID != "" {
		mr.RequestedBy = RequestedByJob
		if job, ok, err := s.Get(ctx, in.JobID); err != nil {
			return MergeRequest{}, err
		} else if ok {
			mr.FixRound = MergeFixRoundOf(job.Spec)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO merge_requests (id, repo, branch, head, note, requested_by, state, created_at, updated_at,
		   source_job_id, after_branches, fix_round)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		mr.ID, mr.Repo, mr.Branch, mr.Head, mr.Note, mr.RequestedBy, mr.State, mr.CreatedAt, mr.UpdatedAt,
		mr.SourceJobID, strings.Join(mr.After, "\n"), mr.FixRound); err != nil {
		return MergeRequest{}, err
	}
	if mr.WaitingOn, err = s.mergeWaitingOn(ctx, mr); err != nil {
		return MergeRequest{}, err
	}
	return mr, nil
}

const mergeCols = `id, repo, branch, head, note, requested_by, job_id, state, result, created_at, updated_at,
	source_job_id, after_branches, fix_round, failure`

func scanMerge(row rowScanner) (MergeRequest, error) {
	var m MergeRequest
	var after string
	err := row.Scan(&m.ID, &m.Repo, &m.Branch, &m.Head, &m.Note, &m.RequestedBy, &m.JobID,
		&m.State, &m.Result, &m.CreatedAt, &m.UpdatedAt,
		&m.SourceJobID, &after, &m.FixRound, &m.Failure)
	if after != "" {
		m.After = strings.Split(after, "\n")
	}
	return m, err
}

// mergeWaitingOn returns the branches of mr.After that have no landed
// merge request in mr.Repo yet (spec §6.11 dependency gate). Only a
// queued request waits; every other state returns nil.
func (s *Store) mergeWaitingOn(ctx context.Context, mr MergeRequest) ([]string, error) {
	if mr.State != MergeQueued {
		return nil, nil
	}
	var waiting []string
	for _, dep := range mr.After {
		var one int
		err := s.db.QueryRowContext(ctx,
			`SELECT 1 FROM merge_requests WHERE repo = ? AND branch = ? AND state = ? LIMIT 1`,
			mr.Repo, dep, MergeLanded).Scan(&one)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			waiting = append(waiting, dep)
		case err != nil:
			return nil, err
		}
	}
	return waiting, nil
}

// queryMerges runs a merge_requests query and fills WaitingOn.
func (s *Store) queryMerges(ctx context.Context, query string, args ...any) ([]MergeRequest, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	out := []MergeRequest{}
	for rows.Next() {
		m, err := scanMerge(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].WaitingOn, err = s.mergeWaitingOn(ctx, out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// QueuedMerges returns every queued request, oldest first, with
// WaitingOn filled.
func (s *Store) QueuedMerges(ctx context.Context) ([]MergeRequest, error) {
	return s.queryMerges(ctx, `SELECT `+mergeCols+` FROM merge_requests WHERE state = ? ORDER BY id ASC`, MergeQueued)
}

// ActiveMergeForBranch returns the queued or merging request for
// repo+branch; ok is false when there is none.
func (s *Store) ActiveMergeForBranch(ctx context.Context, repo, branch string) (MergeRequest, bool, error) {
	list, err := s.queryMerges(ctx,
		`SELECT `+mergeCols+` FROM merge_requests WHERE repo = ? AND branch = ? AND state IN (?, ?) ORDER BY id DESC LIMIT 1`,
		repo, branch, MergeQueued, MergeMerging)
	if err != nil || len(list) == 0 {
		return MergeRequest{}, false, err
	}
	return list[0], true, nil
}

// MergeRequestsBySourceJob returns the requests job jobID filed for its
// own work, oldest first.
func (s *Store) MergeRequestsBySourceJob(ctx context.Context, jobID string) ([]MergeRequest, error) {
	if jobID == "" {
		return nil, nil
	}
	return s.queryMerges(ctx, `SELECT `+mergeCols+` FROM merge_requests WHERE source_job_id = ? ORDER BY id ASC`, jobID)
}

// SetMergeFailure records the classified failure kind of request id.
func (s *Store) SetMergeFailure(ctx context.Context, id, kind string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE merge_requests SET failure = ? WHERE id = ?`, kind, id)
	return err
}

// MergeRequests returns the newest limit requests (default 200), ordered
// oldest first (ids are ULIDs, so id order is creation order).
func (s *Store) MergeRequests(ctx context.Context, limit int) ([]MergeRequest, error) {
	if limit <= 0 {
		limit = 200
	}
	return s.queryMerges(ctx,
		`SELECT `+mergeCols+` FROM (SELECT `+mergeCols+` FROM merge_requests ORDER BY id DESC LIMIT ?) ORDER BY id ASC`, limit)
}

// GetMergeRequest returns one request; ok is false when absent.
func (s *Store) GetMergeRequest(ctx context.Context, id string) (MergeRequest, bool, error) {
	m, err := scanMerge(s.db.QueryRowContext(ctx, `SELECT `+mergeCols+` FROM merge_requests WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return MergeRequest{}, false, nil
	}
	return m, err == nil, err
}

// MergeRequestForJob returns the request whose merge job is jobID; ok is
// false when jobID is not a merge job.
func (s *Store) MergeRequestForJob(ctx context.Context, jobID string) (MergeRequest, bool, error) {
	if jobID == "" {
		return MergeRequest{}, false, nil
	}
	m, err := scanMerge(s.db.QueryRowContext(ctx,
		`SELECT `+mergeCols+` FROM merge_requests WHERE job_id = ? ORDER BY id DESC LIMIT 1`, jobID))
	if errors.Is(err, sql.ErrNoRows) {
		return MergeRequest{}, false, nil
	}
	return m, err == nil, err
}

// OldestQueuedMerge returns the oldest DISPATCHABLE queued request: the
// oldest one whose after branches have all landed (spec §6.11). A request
// still waiting on a dependency is skipped, so it never blocks the queue.
// ok is false when nothing is dispatchable.
func (s *Store) OldestQueuedMerge(ctx context.Context) (MergeRequest, bool, error) {
	queued, err := s.QueuedMerges(ctx)
	if err != nil {
		return MergeRequest{}, false, err
	}
	for _, m := range queued {
		if len(m.WaitingOn) == 0 {
			return m, true, nil
		}
	}
	return MergeRequest{}, false, nil
}

// MergingRequests returns the requests currently merging (at most one by
// the unique index; a slice so reconciliation code stays honest).
func (s *Store) MergingRequests(ctx context.Context) ([]MergeRequest, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+mergeCols+` FROM merge_requests WHERE state = ? ORDER BY id`, MergeMerging)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MergeRequest
	for rows.Next() {
		m, err := scanMerge(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ActiveMergeJobs counts merge jobs (jobs referenced by any merge request)
// that are not yet terminal. The dispatch pass requires zero: a merge job
// from a cancelled request that is somehow still live still holds the
// queue (spec §6.11).
func (s *Store) ActiveMergeJobs(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM jobs j JOIN merge_requests m ON m.job_id = j.id
		 WHERE j.state NOT IN (?, ?, ?, ?, ?)`,
		StateDone, StateTimedOut, StateDead, StateCancelled, StateSuperseded).Scan(&n)
	return n, err
}

// ClaimMerge moves request id from queued to merging with jobID recorded,
// before the merge job is created (spec §6.11 dispatch order: claim, then
// create). claimed is false when the request is no longer queued or
// another request is already merging (the unique index refuses it).
func (s *Store) ClaimMerge(ctx context.Context, id, jobID string) (claimed bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE merge_requests SET state = ?, job_id = ?, updated_at = ? WHERE id = ? AND state = ?`,
		MergeMerging, jobID, s.now(), id, MergeQueued)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			log.Printf("jobs: merge claim %s refused: another request is merging", id)
			return false, nil
		}
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SwapMergeJob points a merging request at its escalation job newJobID,
// compare-and-swapping on the failed job oldJobID. swapped is false when
// the request is no longer merging that job (spec §6.11 escalation).
func (s *Store) SwapMergeJob(ctx context.Context, id, oldJobID, newJobID string) (swapped bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE merge_requests SET job_id = ?, updated_at = ? WHERE id = ? AND state = ? AND job_id = ?`,
		newJobID, s.now(), id, MergeMerging, oldJobID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// TransitionMerge applies from -> to for request id if legal, with a
// compare-and-swap on the observed state, recording result when non-empty.
// Illegal transitions are logged and not applied (the jobs model, §6.1).
func (s *Store) TransitionMerge(ctx context.Context, id, to, result string) (MergeRequest, bool, error) {
	for range 3 {
		cur, ok, err := s.GetMergeRequest(ctx, id)
		if err != nil {
			return MergeRequest{}, false, err
		}
		if !ok {
			return MergeRequest{}, false, fmt.Errorf("%w: %s", ErrMergeNotFound, id)
		}
		if !MergeTransitionLegal(cur.State, to) {
			log.Printf("jobs: ignoring illegal merge transition %s -> %s for %s", cur.State, to, id)
			return cur, false, nil
		}
		if len(result) > maxMergeResultBytes {
			result = truncateUTF8(result, maxMergeResultBytes)
		}
		res, err := s.db.ExecContext(ctx,
			`UPDATE merge_requests SET state = ?, result = CASE WHEN ? = '' THEN result ELSE ? END, updated_at = ?
			 WHERE id = ? AND state = ?`,
			to, result, result, s.now(), id, cur.State)
		if err != nil {
			return MergeRequest{}, false, err
		}
		if n, err := res.RowsAffected(); err != nil {
			return MergeRequest{}, false, err
		} else if n == 0 {
			continue // raced; re-read and re-classify
		}
		m, _, err := s.GetMergeRequest(ctx, id)
		return m, true, err
	}
	return MergeRequest{}, false, fmt.Errorf("jobs: merge transition of %s lost repeated CAS races", id)
}

// FinishMergeForJob closes the merging request whose merge job is jobID
// after the job reached jobState (the onTerminal hook and the loop's
// reconciliation both call it). applied is false when jobID is not a merge
// job, the request is not merging, or the state maps to no terminal.
func (s *Store) FinishMergeForJob(ctx context.Context, jobID, jobState string) (MergeRequest, bool, error) {
	mr, ok, err := s.MergeRequestForJob(ctx, jobID)
	if err != nil || !ok || mr.State != MergeMerging {
		return mr, false, err
	}
	to, ok := MergeStateForJob(jobState)
	if !ok {
		return mr, false, nil
	}
	outcome := s.mergeOutcome(ctx, jobID, jobState)
	if to == MergeLanded {
		// done is not enough: the job's structured result must say the
		// branch merged. Anything else (merged:false, no result, an
		// untyped result) is visible as not shipped.
		job, found, err := s.Get(ctx, jobID)
		if err != nil {
			return mr, false, err
		}
		if !found || !MergeResultMerged(job) {
			to = MergeNeedsAttention
			outcome = "not merged (job done without result.merged=true): " + outcome
		}
	}
	return s.TransitionMerge(ctx, mr.ID, to, truncateUTF8(outcome, maxMergeResultBytes))
}

// MergeResultMerged reports whether the merge job's last reported result
// carries merged:true (the typed MergeResultSchema). Missing, malformed or
// untyped results are false.
func MergeResultMerged(job Job) bool {
	var results []struct {
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal(job.Results, &results) != nil || len(results) == 0 {
		return false
	}
	var body struct {
		Merged *bool `json:"merged"`
	}
	last := results[len(results)-1].Result
	if len(last) == 0 || json.Unmarshal(last, &body) != nil {
		return false
	}
	return body.Merged != nil && *body.Merged
}

// mergeOutcome renders the request's result summary from its job: the
// job's final state, its last result summary, and its last error.
func (s *Store) mergeOutcome(ctx context.Context, jobID, jobState string) string {
	out := "job " + jobID + " " + jobState
	job, ok, err := s.Get(ctx, jobID)
	if err != nil || !ok {
		return out
	}
	var results []struct {
		Summary string `json:"summary"`
	}
	if json.Unmarshal(job.Results, &results) == nil && len(results) > 0 && results[len(results)-1].Summary != "" {
		out += ": " + results[len(results)-1].Summary
	}
	if job.LastError != "" {
		out += " (error: " + job.LastError + ")"
	}
	return out
}

// CancelMergeRequest cancels a queued or merging request (spec §6.11).
// Cancelling a merging request cancels its merge job first (through the
// jobs cancel path, whose terminal hook may itself close the request);
// a terminal request is ErrMergeTerminal, an unknown one ErrMergeNotFound.
func (s *Store) CancelMergeRequest(ctx context.Context, id, reason string) (MergeRequest, error) {
	mr, ok, err := s.GetMergeRequest(ctx, id)
	if err != nil {
		return MergeRequest{}, err
	}
	if !ok {
		return MergeRequest{}, fmt.Errorf("%w: %s", ErrMergeNotFound, id)
	}
	if MergeTerminal(mr.State) {
		return MergeRequest{}, fmt.Errorf("%w: %s is %s", ErrMergeTerminal, id, mr.State)
	}
	msg := "cancelled by operator"
	if reason != "" {
		msg += ": " + reason
	}
	if mr.State == MergeMerging && mr.JobID != "" {
		if _, err := s.Cancel(ctx, mr.JobID, StateCancelled, "merge request "+id+" cancelled"); err != nil &&
			!errors.Is(err, ErrTerminal) && !errors.Is(err, ErrNoSuchJob) {
			return MergeRequest{}, fmt.Errorf("cancel merge job %s: %w", mr.JobID, err)
		}
	}
	got, applied, err := s.TransitionMerge(ctx, id, MergeCancelled, msg)
	if err != nil {
		return MergeRequest{}, err
	}
	if applied || got.State == MergeCancelled {
		return got, nil
	}
	// Raced to another terminal state (the job landed first).
	return MergeRequest{}, fmt.Errorf("%w: %s is %s", ErrMergeTerminal, id, got.State)
}

// MergePaused reads the persisted pause flag and when it last changed.
func (s *Store) MergePaused(ctx context.Context) (paused bool, changedAt string, err error) {
	var p int
	err = s.db.QueryRowContext(ctx, `SELECT paused, updated_at FROM merge_queue WHERE id = 1`).Scan(&p, &changedAt)
	return p != 0, changedAt, err
}

// SetMergePaused persists the pause flag (spec §6.11): paused holds new
// dispatches; an in-flight merge still finishes.
func (s *Store) SetMergePaused(ctx context.Context, paused bool) error {
	flag := 0
	if paused {
		flag = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE merge_queue SET paused = ?, updated_at = ? WHERE id = 1`, flag, s.now())
	return err
}

// MergeQueueView is the GET /v1/merge-requests body.
func (s *Store) MergeQueueView(ctx context.Context, limit int) (MergeQueue, error) {
	paused, at, err := s.MergePaused(ctx)
	if err != nil {
		return MergeQueue{}, err
	}
	reqs, err := s.MergeRequests(ctx, limit)
	if err != nil {
		return MergeQueue{}, err
	}
	return MergeQueue{Paused: paused, PausedChangedAt: at, Requests: reqs}, nil
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// MergeRequestsHandler exposes the merge queue API (spec §6.9, §6.11):
// POST /v1/merge-requests {branch, head, repo?, note?} -> 201 queued;
// GET /v1/merge-requests?limit= -> {paused, paused_changed_at?, requests}
// oldest first; POST /v1/merge-requests/{id}/cancel {reason?};
// POST /v1/merge-requests/pause and /resume -> the queue view. onChange,
// when set, is told after every successful mutation (the factory kicks
// its dispatch loop). Errors follow the §10 model.
func (s *Store) MergeRequestsHandler(onChange func()) http.Handler {
	changed := func() {
		if onChange != nil {
			onChange()
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/merge-requests", func(w http.ResponseWriter, r *http.Request) {
		var in NewMergeRequest
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "malformed body: "+err.Error(), false)
			return
		}
		// job_id marks a job-owned request (a foreman relays its worker's
		// request_merge here, spec §9.2); CreateMergeRequest then records
		// requested_by "job".
		in.RequestedBy = "operator"
		mr, err := s.CreateMergeRequest(r.Context(), in)
		if err != nil {
			writeMergeError(w, err)
			return
		}
		changed()
		writeJSON(w, http.StatusCreated, mr)
	})
	mux.HandleFunc("GET /v1/merge-requests", func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 || n > 1000 {
				writeError(w, http.StatusBadRequest, "bad_request", "limit must be an integer in [1, 1000]", false)
				return
			}
			limit = n
		}
		view, err := s.MergeQueueView(r.Context(), limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
			return
		}
		writeJSON(w, http.StatusOK, view)
	})
	mux.HandleFunc("POST /v1/merge-requests/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Reason string `json:"reason"`
		}
		if raw, err := io.ReadAll(r.Body); err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "read body: "+err.Error(), false)
			return
		} else if strings.TrimSpace(string(raw)) != "" {
			if err := json.Unmarshal(raw, &in); err != nil {
				writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error(), false)
				return
			}
		}
		mr, err := s.CancelMergeRequest(r.Context(), r.PathValue("id"), in.Reason)
		if err != nil {
			writeMergeError(w, err)
			return
		}
		changed()
		writeJSON(w, http.StatusOK, mr)
	})
	setPaused := func(paused bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if err := s.SetMergePaused(r.Context(), paused); err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
				return
			}
			changed()
			view, err := s.MergeQueueView(r.Context(), 200)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
				return
			}
			writeJSON(w, http.StatusOK, view)
		}
	}
	mux.HandleFunc("POST /v1/merge-requests/pause", setPaused(true))
	mux.HandleFunc("POST /v1/merge-requests/resume", setPaused(false))
	return mux
}

// writeMergeError maps merge store errors to the §10 model.
func writeMergeError(w http.ResponseWriter, err error) {
	var bad *errBadMerge
	switch {
	case errors.As(err, &bad):
		writeError(w, http.StatusBadRequest, "bad_request", bad.Error(), false)
	case errors.Is(err, ErrMergeNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error(), false)
	case errors.Is(err, ErrMergeTerminal), errors.Is(err, ErrMergeNotDismissable):
		writeError(w, http.StatusConflict, "already_terminal", err.Error(), false)
	case errors.Is(err, ErrMergeDuplicate):
		writeError(w, http.StatusConflict, "duplicate_request", err.Error(), false)
	default:
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
	}
}
