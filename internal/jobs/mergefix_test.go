package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

// landMR drives a queued request to landed through a claimed merge job.
func landMR(t *testing.T, s *Store, mr MergeRequest) {
	t.Helper()
	ctx := context.Background()
	if ok, err := s.ClaimMerge(ctx, mr.ID, wire.NewJobID()); !ok || err != nil {
		t.Fatalf("claim %s: %v %v", mr.ID, ok, err)
	}
	if _, applied, err := s.TransitionMerge(ctx, mr.ID, MergeLanded, "merged"); !applied || err != nil {
		t.Fatalf("land %s: %v %v", mr.ID, applied, err)
	}
}

// TestJobOwnedMergeRequest: a request filed with a job id is job-owned
// (requested_by job, source_job_id), a plain job starts at fix round 0,
// and a merge-fix job's request inherits the job's round whatever branch
// it names (the loop bound cannot be reset).
func TestJobOwnedMergeRequest(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	plain := wire.NewJobSpec("change code", "factory")
	plain.ID = wire.NewJobID()
	if err := s.Create(ctx, plain); err != nil {
		t.Fatal(err)
	}
	mr, err := s.CreateMergeRequest(ctx, NewMergeRequest{Branch: "feat/x", Head: testHead, JobID: plain.ID, RequestedBy: "operator"})
	if err != nil {
		t.Fatal(err)
	}
	if mr.RequestedBy != RequestedByJob || mr.SourceJobID != plain.ID || mr.FixRound != 0 {
		t.Fatalf("job-owned request = %+v", mr)
	}
	owned, err := s.MergeRequestsBySourceJob(ctx, plain.ID)
	if err != nil || len(owned) != 1 || owned[0].ID != mr.ID {
		t.Fatalf("MergeRequestsBySourceJob = %+v, %v", owned, err)
	}
	// The merge job of a request is NOT its source job.
	if _, ok, _ := s.MergeRequestForJob(ctx, plain.ID); ok {
		t.Fatal("source job was taken for a merge job")
	}

	src, _, _ := s.Get(ctx, plain.ID)
	for _, branch := range []string{"feat/x2", "feat/other"} {
		fix := MergeFixJobSpec(mr, MergeFailureCI, src, src, wire.NewJobID(), 2)
		if err := s.Create(ctx, fix); err != nil {
			t.Fatal(err)
		}
		got, err := s.CreateMergeRequest(ctx, NewMergeRequest{Branch: branch, Head: testHead, JobID: fix.ID})
		if err != nil {
			t.Fatal(err)
		}
		if got.FixRound != 2 {
			t.Errorf("request for %s by a round-2 fix job has fix_round %d, want 2", branch, got.FixRound)
		}
	}

	// An unknown job id is still recorded (a spawn_child child may not
	// be registered yet); an unusable one is refused.
	if got, err := s.CreateMergeRequest(ctx, NewMergeRequest{Branch: "feat/y", Head: testHead, JobID: "job_unknown"}); err != nil || got.SourceJobID != "job_unknown" {
		t.Errorf("unknown job: %+v, %v", got, err)
	}
	if _, err := s.CreateMergeRequest(ctx, NewMergeRequest{Branch: "feat/z", Head: testHead, JobID: "job; rm -rf"}); !IsBadMergeRequest(err) {
		t.Errorf("bad job id: err = %v, want a validation error", err)
	}
}

// TestJobReRequestSupersedesQueued: a job re-requesting its branch at a
// new head replaces its still-queued request; a merging request is never
// replaced, and a non-job requester keeps the plain duplicate refusal.
func TestJobReRequestSupersedesQueued(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	first, err := s.CreateMergeRequest(ctx, NewMergeRequest{Branch: "feat/x", Head: testHead, JobID: "job_a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateMergeRequest(ctx, NewMergeRequest{Branch: "feat/x", Head: testHead, RequestedBy: "planner"}); !errors.Is(err, ErrMergeDuplicate) {
		t.Fatalf("planner duplicate: err = %v, want ErrMergeDuplicate", err)
	}
	second, err := s.CreateMergeRequest(ctx, NewMergeRequest{Branch: "feat/x", Head: "89abcdef", JobID: "job_a"})
	if err != nil {
		t.Fatal(err)
	}
	old, _, _ := s.GetMergeRequest(ctx, first.ID)
	if old.State != MergeCancelled || !strings.Contains(old.Result, "superseded") || !strings.Contains(old.Result, "89abcdef") {
		t.Fatalf("replaced request = %+v", old)
	}
	if second.State != MergeQueued || second.Head != "89abcdef" {
		t.Fatalf("new request = %+v", second)
	}
	if ok, err := s.ClaimMerge(ctx, second.ID, wire.NewJobID()); !ok || err != nil {
		t.Fatal(ok, err)
	}
	_, err = s.CreateMergeRequest(ctx, NewMergeRequest{Branch: "feat/x", Head: testHead, JobID: "job_a"})
	if !errors.Is(err, ErrMergeDuplicate) || !strings.Contains(err.Error(), "merging") {
		t.Fatalf("re-request while merging: err = %v, want ErrMergeDuplicate naming the merging request", err)
	}
	if cur, _, _ := s.GetMergeRequest(ctx, second.ID); cur.State != MergeMerging {
		t.Fatalf("merging request was touched: %+v", cur)
	}
}

// TestMergeAfterValidation covers the after list rules.
func TestMergeAfterValidation(t *testing.T) {
	many := make([]string, maxMergeAfter+1)
	for i := range many {
		many[i] = "feat/dep" + string(rune('a'+i))
	}
	cases := []struct {
		name  string
		after []string
		want  []string // nil = validation error
		bad   bool
	}{
		{"none", nil, nil, false},
		{"trim, drop empty, dedupe", []string{" feat/a ", "", "feat/a", "feat/b"}, []string{"feat/a", "feat/b"}, false},
		{"self", []string{"feat/x"}, nil, true},
		{"unsafe name", []string{"feat/a; rm"}, nil, true},
		{"dotdot", []string{"feat/../a"}, nil, true},
		{"too many", many, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openStore(t)
			mr, err := s.CreateMergeRequest(context.Background(), NewMergeRequest{Branch: "feat/x", Head: testHead, After: tc.after})
			if tc.bad {
				if !IsBadMergeRequest(err) {
					t.Fatalf("err = %v, want a validation error", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(mr.After, tc.want) || !reflect.DeepEqual(mr.WaitingOn, tc.want) {
				t.Fatalf("after = %v waiting_on = %v err = %v, want %v", mr.After, mr.WaitingOn, err, tc.want)
			}
		})
	}
}

// TestMergeDependencyGate: a request with after is skipped by the
// dispatch pick until every named branch has a landed request; it never
// blocks younger requests; a failed dependency keeps it waiting.
func TestMergeDependencyGate(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	dependent, err := s.CreateMergeRequest(ctx, NewMergeRequest{Branch: "feat/ui", Head: testHead, After: []string{"feat/api", "feat/db"}})
	if err != nil {
		t.Fatal(err)
	}
	api := mustMR(t, s, "feat/api")
	pick := func() string {
		t.Helper()
		m, ok, err := s.OldestQueuedMerge(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			return ""
		}
		return m.ID
	}
	waiting := func() []string {
		t.Helper()
		view, err := s.MergeQueueView(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range view.Requests {
			if m.ID == dependent.ID {
				return m.WaitingOn
			}
		}
		t.Fatal("dependent not in view")
		return nil
	}
	if got := pick(); got != api.ID {
		t.Fatalf("picked %q, want the younger undependent %s (the dependent must not block the queue)", got, api.ID)
	}
	landMR(t, s, api)
	if got := pick(); got != "" {
		t.Fatalf("picked %q with feat/db not landed", got)
	}
	if got := waiting(); !reflect.DeepEqual(got, []string{"feat/db"}) {
		t.Fatalf("waiting_on = %v, want [feat/db]", got)
	}
	// A dependency that fails does not release the dependent.
	db := mustMR(t, s, "feat/db")
	if ok, err := s.ClaimMerge(ctx, db.ID, wire.NewJobID()); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if _, _, err := s.TransitionMerge(ctx, db.ID, MergeNeedsAttention, "checks red"); err != nil {
		t.Fatal(err)
	}
	if got := pick(); got != "" {
		t.Fatalf("picked %q after the dependency FAILED", got)
	}
	landMR(t, s, mustMR(t, s, "feat/db"))
	if got := pick(); got != dependent.ID {
		t.Fatalf("picked %q, want the released dependent %s", got, dependent.ID)
	}
	if got := waiting(); len(got) != 0 {
		t.Fatalf("waiting_on = %v, want none", got)
	}
	// A landed request for the same branch name in ANOTHER repo is no
	// dependency.
	other, err := s.CreateMergeRequest(ctx, NewMergeRequest{Repo: "acme/other", Branch: "feat/ui", Head: testHead, After: []string{"feat/api"}})
	if err != nil || !reflect.DeepEqual(other.WaitingOn, []string{"feat/api"}) {
		t.Fatalf("cross-repo dependent = %+v, %v", other, err)
	}
}

// TestClassifyMergeFailure: result.stage wins, the pinned summary and
// error prefixes are the fallback, anything else is "other".
func TestClassifyMergeFailure(t *testing.T) {
	type entry struct {
		Summary string         `json:"summary"`
		Result  map[string]any `json:"result,omitempty"`
		Error   map[string]any `json:"error,omitempty"`
	}
	cases := []struct {
		name    string
		results []entry
		want    string
		fixable bool
	}{
		{"no report", nil, MergeFailureOther, false},
		{"stage ci", []entry{{Summary: "x", Result: map[string]any{"stage": "ci"}}}, MergeFailureCI, true},
		{"stage wins over summary", []entry{{Summary: "deploy: unit failed", Result: map[string]any{"stage": "conflict"}}}, MergeFailureConflict, true},
		{"stage deploy", []entry{{Summary: "x", Result: map[string]any{"stage": "deploy"}}}, MergeFailureDeploy, false},
		{"unknown stage falls back", []entry{{Summary: "head drift: origin moved", Result: map[string]any{"stage": "weird"}}}, MergeFailureHeadDrift, true},
		{"summary checks red", []entry{{Summary: "checks red after rerun: TestX"}}, MergeFailureCI, true},
		{"summary conflict", []entry{{Summary: "Conflict: a.go"}}, MergeFailureConflict, true},
		{"summary nothing to merge", []entry{{Summary: "nothing to merge"}}, MergeFailureNothing, false},
		{"summary preflight", []entry{{Summary: "preflight: gh auth"}}, MergeFailurePreflight, false},
		{"summary deploy", []entry{{Summary: "deploy: health check; rolled back: yes"}}, MergeFailureDeploy, false},
		{"error message prefix", []entry{{Summary: "stopped", Error: map[string]any{"kind": "other", "message": "conflict: b.go"}}}, MergeFailureConflict, true},
		{"last entry decides", []entry{{Summary: "conflict: a.go"}, {Summary: "killed: idle"}}, MergeFailureOther, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var job Job
			if tc.results != nil {
				job.Results, _ = json.Marshal(tc.results)
			}
			got := ClassifyMergeFailure(job)
			if got != tc.want || MergeFailureFixable(got) != tc.fixable {
				t.Fatalf("kind = %q fixable = %v, want %q / %v", got, MergeFailureFixable(got), tc.want, tc.fixable)
			}
		})
	}
}

// TestMergeFixJobSpec: the fix job runs like its source job, on the same
// foreman, with a code-generated task that quotes the origin task as data
// and names the branch, the bound and the request_merge step.
func TestMergeFixJobSpec(t *testing.T) {
	src := wire.NewJobSpec("Add the countdown to the usage panel.", "factory")
	src.ID = "job_src"
	src.Tier, src.Model, src.TimeoutS, src.Priority = wire.TierHardest, "claude-x", 5400, 4
	src.Requires, src.Secrets = []string{"repo:factory"}, []string{"gh-token"}
	src.Budget.MaxCostUSD = 7
	mr := MergeRequest{ID: "mr_1", Repo: DefaultMergeRepo, Branch: "feat/countdown", Head: testHead, Result: `job j failed: checks red after rerun: TestX`}

	cases := []struct {
		name        string
		pin, ranOn  string
		wantForeman string
	}{
		{"pinned source keeps its pin", "foreman:alpha", "foreman:beta", "foreman:alpha"},
		{"unpinned source pins to where it ran", "", "beta", "foreman:beta"},
		{"never ran anywhere: unpinned", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := src
			spec.Foreman = tc.pin
			source := Job{ID: src.ID, Spec: spec, ForemanID: tc.ranOn}
			origin := Job{ID: "job_origin", Spec: wire.NewJobSpec("ORIGINAL TASK TEXT", "factory")}
			fix := MergeFixJobSpec(mr, MergeFailureCI, source, origin, "job_fix", 2)
			if err := fix.Validate(); err != nil {
				t.Fatalf("fix spec invalid: %v", err)
			}
			if fix.ID != "job_fix" || fix.CreatedBy != MergeFixCreatedBy || fix.Foreman != tc.wantForeman ||
				fix.Tier != src.Tier || fix.Model != src.Model || fix.TimeoutS != src.TimeoutS || fix.Priority != src.Priority ||
				fix.Budget.MaxCostUSD != 7 || !reflect.DeepEqual(fix.Requires, src.Requires) || !reflect.DeepEqual(fix.Secrets, src.Secrets) {
				t.Fatalf("fix spec = %+v", fix)
			}
			p, ok := MergeFixPayloadOf(fix)
			if !ok || p.FixRound != 2 || p.Branch != mr.Branch || p.MergeRequestID != mr.ID || p.OriginJobID != "job_origin" || p.Failure != MergeFailureCI {
				t.Fatalf("payload = %+v ok=%v", p, ok)
			}
			if MergeFixRoundOf(fix) != 2 || MergeFixRoundOf(src) != 0 {
				t.Fatal("MergeFixRoundOf wrong")
			}
			for _, want := range []string{
				"merge-fix job 2 of at most 2", "mr_1", "feat/countdown", "checks red after rerun",
				"request_merge", "never force-push", "Do not weaken or delete tests", "/home/price/temp/factory",
				"ORIGINAL TASK TEXT", "error.kind=validation",
			} {
				if !strings.Contains(fix.Task, want) {
					t.Errorf("fix task lacks %q", want)
				}
			}
			if strings.Contains(fix.Task, "Add the countdown") {
				t.Error("fix task quotes the source task, want the ORIGIN task")
			}
		})
	}
	// A planner job that merely carries a look-alike payload is no fix job.
	fake := wire.NewJobSpec("x", "factory")
	fake.Payload = json.RawMessage(`{"kind":"merge_fix","fix_round":0}`)
	if _, ok := MergeFixPayloadOf(fake); ok || MergeFixRoundOf(fake) != 0 {
		t.Error("a non-factory:merge_fix job was accepted as a fix job")
	}
}

// TestMergeRequestsHTTPJobOwned: POST /v1/merge-requests with job_id and
// after (the foreman's request_merge relay) stores a job-owned, gated
// request and reports what it waits on.
func TestMergeRequestsHTTPJobOwned(t *testing.T) {
	s := openStore(t)
	kicks := 0
	srv := httptest.NewServer(s.MergeRequestsHandler(func() { kicks++ }))
	defer srv.Close()
	body := `{"branch":"feat/ui","head":"` + testHead + `","job_id":"job_a","after":["feat/api"]}`
	resp, err := http.Post(srv.URL+"/v1/merge-requests", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var mr MergeRequest
	if err := json.NewDecoder(resp.Body).Decode(&mr); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated || mr.RequestedBy != RequestedByJob || mr.SourceJobID != "job_a" ||
		!reflect.DeepEqual(mr.After, []string{"feat/api"}) || !reflect.DeepEqual(mr.WaitingOn, []string{"feat/api"}) || kicks != 1 {
		t.Fatalf("status %d, request %+v, kicks %d", resp.StatusCode, mr, kicks)
	}
	// Without job_id the request is the operator's, as before.
	resp2, err := http.Post(srv.URL+"/v1/merge-requests", "application/json",
		strings.NewReader(`{"branch":"feat/api","head":"`+testHead+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var op MergeRequest
	_ = json.NewDecoder(resp2.Body).Decode(&op)
	if op.RequestedBy != "operator" || op.SourceJobID != "" {
		t.Fatalf("operator request = %+v", op)
	}
}
