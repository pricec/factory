// Conflict coordinator (conflict.go): the second admission stage. Held ->
// released lifecycle keyed on the blocker's MERGE, docs-soft overlap, P0 and
// merge-job exemptions, deadlock guard, max wait, cancel while held,
// restart persistence, usage-vs-conflict precedence.
package factoryapp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

func conflictApp(t *testing.T) (*App, *gateClock) {
	t.Helper()
	clk := &gateClock{t: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	t.Cleanup(func() { a.Close() })
	return a, clk
}

// postTouch creates a job over POST /v1/jobs and returns id, state, held_reason.
func postTouch(t *testing.T, a *App, in map[string]any) (id, state, reason string) {
	t.Helper()
	if in["task"] == nil {
		in["task"] = "work"
	}
	code, out := doJSON(t, a, "POST", "/v1/jobs", in)
	if code != http.StatusCreated {
		t.Fatalf("create %v: %d %v", in, code, out)
	}
	id, _ = out["job_id"].(string)
	state, _ = out["state"].(string)
	reason, _ = out["held_reason"].(string)
	return id, state, reason
}

// doneWithMerge drives a job to done and queues its merge request; returns the
// request id.
func doneWithMerge(t *testing.T, a *App, id string) string {
	t.Helper()
	ctx := context.Background()
	if err := a.jobs.Dispatch(ctx, id, "foreman:alpha"); err != nil {
		t.Fatal(err)
	}
	if err := a.jobs.ApplyEvent(ctx, id, "started"); err != nil {
		t.Fatal(err)
	}
	if err := a.jobs.ApplyResult(ctx, wire.JobResult{JobID: id, Status: "done", Summary: "ok"}); err != nil {
		t.Fatal(err)
	}
	mr, err := a.jobs.CreateMergeRequest(ctx, jobs.NewMergeRequest{Branch: "br-" + id, Head: "abcdef1", Note: "n", JobID: id})
	if err != nil {
		t.Fatal(err)
	}
	return mr.ID
}

func landMerge(t *testing.T, a *App, mrID, to string) {
	t.Helper()
	ctx := context.Background()
	if ok, err := a.jobs.ClaimMerge(ctx, mrID, "job_merger_"+mrID); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if _, ok, err := a.jobs.TransitionMerge(ctx, mrID, to, "x"); err != nil || !ok {
		t.Fatalf("transition: %v %v", ok, err)
	}
}

func TestConflictAdmissionTable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		first      []string // the already-active job's touches
		second     []string
		secondP0   bool
		createdBy  string
		wantHeld   bool
		wantReason string
	}{
		{name: "disjoint admitted", first: []string{"internal/a/**"}, second: []string{"internal/b/**"}},
		{
			name: "overlapping held", first: []string{"internal/jobs/**"}, second: []string{"internal/jobs/jobs.go"},
			wantHeld: true, wantReason: "conflict: waits for %s (internal/jobs/**)",
		},
		{name: "docs-only overlap admitted", first: []string{"docs/spec.md"}, second: []string{"docs/**"}},
		{name: "markdown overlap admitted", first: []string{"README.md"}, second: []string{"**/*.md"}},
		{
			name: "docs plus code overlap held", first: []string{"docs/spec.md", "internal/x.go"}, second: []string{"docs/spec.md", "internal/x.go"},
			wantHeld: true, wantReason: "conflict: waits for %s (internal/x.go)",
		},
		{name: "P0 bypasses", first: []string{"internal/x.go"}, second: []string{"internal/x.go"}, secondP0: true},
		{name: "no touches never held", first: []string{"*"}, second: nil},
		{
			name: "star holds everything that declares touches", first: []string{"*"}, second: []string{"cmd/x.go"},
			wantHeld: true, wantReason: "conflict: waits for %s (*)",
		},
		{
			name: "star candidate waits on any declared job", first: []string{"cmd/x.go"}, second: []string{"*"},
			wantHeld: true, wantReason: "conflict: waits for %s (cmd/x.go)",
		},
		{name: "no-touches blocker blocks nobody", first: nil, second: []string{"cmd/x.go"}},
		{name: "merge job exempt", first: []string{"internal/x.go"}, second: []string{"internal/x.go"}, createdBy: jobs.MergeJobCreatedBy},
		{name: "merge-fix job exempt", first: []string{"internal/x.go"}, second: []string{"internal/x.go"}, createdBy: jobs.MergeFixCreatedBy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := conflictApp(t)
			ctx := context.Background()
			first, state, _ := postTouch(t, a, map[string]any{"touches": tc.first})
			if state != jobs.StatePending {
				t.Fatalf("first job state %s", state)
			}
			if tc.createdBy != "" {
				spec := wire.NewJobSpec("merge", tc.createdBy)
				spec.Touches = tc.second
				hold, reason := a.jobs.Admit(ctx, spec, "")
				if hold != tc.wantHeld {
					t.Fatalf("hold=%v reason=%q", hold, reason)
				}
				return
			}
			in := map[string]any{"touches": tc.second}
			if tc.secondP0 {
				task, err := a.tasks.CreateTask(ctx, "urgent", 0)
				if err != nil {
					t.Fatal(err)
				}
				in["task_id"] = task.ID
			}
			_, state, reason := postTouch(t, a, in)
			if held := state == jobs.StateHeld; held != tc.wantHeld {
				t.Fatalf("state %s reason %q, wantHeld %v", state, reason, tc.wantHeld)
			}
			if tc.wantHeld {
				if want := strings.Replace(tc.wantReason, "%s", first, 1); reason != want {
					t.Fatalf("reason %q, want %q", reason, want)
				}
			}
		})
	}
}

func TestConflictReleaseWaitsForMergeToLand(t *testing.T) {
	a, _ := conflictApp(t)
	ctx := context.Background()
	task, err := a.tasks.CreateTask(ctx, "t", 3)
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := postTouch(t, a, map[string]any{"touches": []string{"internal/jobs/**"}})
	second, state, reason := postTouch(t, a, map[string]any{"touches": []string{"internal/jobs/held.go"}, "task_id": task.ID})
	if state != jobs.StateHeld || !strings.HasPrefix(reason, "conflict: waits for "+first) {
		t.Fatalf("second: %s %q", state, reason)
	}
	// "waiting on job_X" lands on the task thread.
	if !threadHas(t, a, task.ID, `"action":"waiting"`, first) {
		t.Fatalf("no waiting note on the task thread")
	}
	pass := func(wantReleased int) {
		t.Helper()
		if n, err := a.ReleasePass(ctx); err != nil || n != wantReleased {
			t.Fatalf("ReleasePass = %d %v, want %d", n, err, wantReleased)
		}
	}
	pass(0) // blocker pending
	mr := doneWithMerge(t, a, first)
	pass(0) // blocker done, merge queued: done is not landed
	if j := mustJob(t, a, second); j.State != jobs.StateHeld {
		t.Fatalf("released while the merge was only queued: %s", j.State)
	}
	landMerge(t, a, mr, jobs.MergeLanded)
	pass(1)
	if j := mustJob(t, a, second); j.State != jobs.StatePending || j.HeldReason != "" {
		t.Fatalf("after landing: %s %q", j.State, j.HeldReason)
	}
	if !threadHas(t, a, task.ID, `"action":"released"`, second) {
		t.Fatalf("no released note on the task thread")
	}
}

func TestConflictReleaseOnCancelledOrFailedMerge(t *testing.T) {
	for _, to := range []string{jobs.MergeCancelled, jobs.MergeNeedsAttention} {
		t.Run(to, func(t *testing.T) {
			a, _ := conflictApp(t)
			ctx := context.Background()
			first, _, _ := postTouch(t, a, map[string]any{"touches": []string{"x/**"}})
			second, _, _ := postTouch(t, a, map[string]any{"touches": []string{"x/y.go"}})
			mr := doneWithMerge(t, a, first)
			landMerge(t, a, mr, to)
			if n, err := a.ReleasePass(ctx); err != nil || n != 1 || mustJob(t, a, second).State != jobs.StatePending {
				t.Fatalf("release after %s: %d %v", to, n, err)
			}
		})
	}
}

func TestConflictReleaseWhenBlockerTerminalWithoutMerge(t *testing.T) {
	a, _ := conflictApp(t)
	ctx := context.Background()
	first, _, _ := postTouch(t, a, map[string]any{"touches": []string{"x/**"}})
	second, _, _ := postTouch(t, a, map[string]any{"touches": []string{"x/y.go"}})
	if code, _ := doJSON(t, a, "POST", "/v1/jobs/"+first+"/cancel", nil); code != http.StatusOK {
		t.Fatalf("cancel blocker: %d", code)
	}
	if n, _ := a.ReleasePass(ctx); n != 1 || mustJob(t, a, second).State != jobs.StatePending {
		t.Fatalf("not released after the blocker was cancelled")
	}
}

func TestConflictChainIsFIFO(t *testing.T) {
	a, _ := conflictApp(t)
	ctx := context.Background()
	j1, _, _ := postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	j2, s2, r2 := postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	j3, s3, r3 := postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	if s2 != jobs.StateHeld || s3 != jobs.StateHeld || !strings.Contains(r2, j1) || !strings.Contains(r3, j1) {
		t.Fatalf("chain: %s %q / %s %q", s2, r2, s3, r3)
	}
	landMerge(t, a, doneWithMerge(t, a, j1), jobs.MergeLanded)
	// One pass releases only the older of the two: the released job becomes
	// pending, which blocks the younger one in the same pass.
	if n, err := a.ReleasePass(ctx); err != nil || n != 1 {
		t.Fatalf("pass: %d %v", n, err)
	}
	if mustJob(t, a, j2).State != jobs.StatePending || mustJob(t, a, j3).State != jobs.StateHeld {
		t.Fatalf("j2=%s j3=%s", mustJob(t, a, j2).State, mustJob(t, a, j3).State)
	}
	if r := mustJob(t, a, j3).HeldReason; !strings.Contains(r, j2) {
		t.Fatalf("j3 should now wait on j2: %q", r)
	}
}

func TestConflictNeverWaitsOnNewerJob(t *testing.T) {
	a, clk := conflictApp(t)
	ctx := context.Background()
	setUsage(a, clk, 96) // RED: the older job is usage-held
	older, state, reason := postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	if state != jobs.StateHeld || !strings.HasPrefix(reason, "usage RED") {
		t.Fatalf("older: %s %q", state, reason)
	}
	clk.Add(time.Second)
	newer := wire.NewJobSpec("newer", "test")
	newer.Touches = []string{"f.go"}
	if err := a.jobs.Create(ctx, newer); err != nil { // bypasses admission: already running
		t.Fatal(err)
	}
	setUsage(a, clk, 5)
	a.gate.Evaluate(ctx) // registers the drop; RED hysteresis is 10 min
	clk.Add(11 * time.Minute)
	setUsage(a, clk, 5)
	if n, err := a.ReleasePass(ctx); err != nil || n != 1 || mustJob(t, a, older).State != jobs.StatePending {
		t.Fatalf("older job must not wait on the newer one: %d %v", n, err)
	}
}

func TestConflictMaxWaitFallthrough(t *testing.T) {
	a, clk := conflictApp(t)
	ctx := context.Background()
	task, _ := a.tasks.CreateTask(ctx, "t", 3)
	postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	second, _, _ := postTouch(t, a, map[string]any{"touches": []string{"f.go"}, "task_id": task.ID})
	if n, _ := a.ReleasePass(ctx); n != 0 { // starts the wait clock
		t.Fatalf("released early")
	}
	clk.Add(DefaultConflictMaxWait - time.Minute)
	if n, _ := a.ReleasePass(ctx); n != 0 {
		t.Fatalf("released before the max wait")
	}
	clk.Add(2 * time.Minute)
	if n, err := a.ReleasePass(ctx); err != nil || n != 1 || mustJob(t, a, second).State != jobs.StatePending {
		t.Fatalf("not released after the max wait: %d %v", n, err)
	}
	if !threadHas(t, a, task.ID, `"action":"timed_out"`, second) {
		t.Fatalf("no timeout note on the task thread")
	}
}

func TestConflictCancelWhileHeld(t *testing.T) {
	a, _ := conflictApp(t)
	postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	second, state, _ := postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	if state != jobs.StateHeld {
		t.Fatalf("state %s", state)
	}
	code, out := doJSON(t, a, "POST", "/v1/jobs/"+second+"/cancel", map[string]any{"reason": "no"})
	if code != http.StatusOK || out["state"] != jobs.StateCancelled {
		t.Fatalf("cancel: %d %v", code, out)
	}
	if n, _ := a.ReleasePass(context.Background()); n != 0 {
		t.Fatalf("released a cancelled job")
	}
}

func TestConflictHeldSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	clk := &gateClock{t: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	a := newGateApp(t, dir, clk)
	first, _, _ := postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	second, state, _ := postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	if state != jobs.StateHeld {
		t.Fatalf("state %s", state)
	}
	a.ReleasePass(ctx) //nolint:errcheck // starts the persisted wait clock
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b := newGateApp(t, dir, clk)
	defer b.Close()
	if j := mustJob(t, b, second); j.State != jobs.StateHeld || j.HeldReason != "conflict: waits for "+first+" (f.go)" {
		t.Fatalf("after restart: %s %q", j.State, j.HeldReason)
	}
	if since, _ := b.jobs.ConflictSince(ctx, second); since == "" {
		t.Fatalf("wait start lost across restart")
	}
	if n, _ := b.ReleasePass(ctx); n != 0 {
		t.Fatalf("released while the blocker is still active")
	}
	landMerge(t, b, doneWithMerge(t, b, first), jobs.MergeLanded)
	if n, _ := b.ReleasePass(ctx); n != 1 {
		t.Fatalf("not released after restart + landing")
	}
}

func TestConflictUsagePrecedenceAndCounter(t *testing.T) {
	a, clk := conflictApp(t)
	ctx := context.Background()
	postTouch(t, a, map[string]any{"touches": []string{"f.go"}}) // active blocker
	setUsage(a, clk, 96)
	second, state, reason := postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	if state != jobs.StateHeld || !strings.HasPrefix(reason, "usage RED") {
		t.Fatalf("both would hold: want the usage reason, got %s %q", state, reason)
	}
	// Usage clears: the same job is now held for the conflict.
	setUsage(a, clk, 5)
	a.gate.Evaluate(ctx) // registers the drop; RED hysteresis is 10 min
	clk.Add(11 * time.Minute)
	setUsage(a, clk, 5)
	if n, _ := a.ReleasePass(ctx); n != 0 {
		t.Fatalf("released despite the conflict")
	}
	if j := mustJob(t, a, second); !strings.HasPrefix(j.HeldReason, "conflict: waits for ") {
		t.Fatalf("reason %q", j.HeldReason)
	}
	// A usage-held job as well: the counter shows the breakdown.
	setUsage(a, clk, 99)
	postTouch(t, a, map[string]any{"task": "no files"})
	code, out := doJSON(t, a, "GET", "/v1/throttle", nil)
	by, _ := out["held_by"].(map[string]any)
	if code != 200 || out["held"].(float64) != 2 || by["conflict"] != 1.0 || by["usage"] != 1.0 {
		t.Fatalf("gate view: %d %v", code, out)
	}
}

func TestConflictKillSwitch(t *testing.T) {
	clk := &gateClock{t: time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk, func(c *Config) { c.Throttle.ConflictOff = true })
	defer a.Close()
	postTouch(t, a, map[string]any{"touches": []string{"f.go"}})
	if _, state, _ := postTouch(t, a, map[string]any{"touches": []string{"f.go"}}); state == jobs.StateHeld {
		t.Fatal("FACTORY_CONFLICT=off must not hold")
	}
}

func TestMergeFixJobKeepsChainFootprint(t *testing.T) {
	origin := jobs.Job{ID: "job_o", Spec: wire.JobSpec{Task: "t", Touches: []string{"a/**"}}}
	spec := jobs.MergeFixJobSpec(jobs.MergeRequest{ID: "mr_1", Branch: "b", Head: "abcdef1"}, "ci", origin, origin, "job_f", 1)
	if len(spec.Touches) != 1 || spec.Touches[0] != "a/**" {
		t.Fatalf("fix job touches %v", spec.Touches)
	}
}

// threadHas reports whether the task thread holds a notify.conflict note
// whose payload contains every want substring.
func threadHas(t *testing.T, a *App, taskID string, want ...string) bool {
	t.Helper()
	msgs, err := a.inbox.ThreadMessages(context.Background(), taskID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range msgs {
		if m.Type != notifyConflictType {
			continue
		}
		ok := true
		for _, w := range want {
			ok = ok && strings.Contains(string(m.Payload), w)
		}
		if ok {
			return true
		}
	}
	return false
}
