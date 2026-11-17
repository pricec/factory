package cigate

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeGH models one workflow run (id 7) whose attempts are scripted.
type fakeGH struct {
	t        *testing.T
	runs     []Run
	logs     map[int64][]string // attempt-indexed failed logs per run id
	onRerun  func(f *fakeGH)    // mutates runs when a rerun is requested
	reruns   []string
	listErr  error
	listings int
}

func (f *fakeGH) Run(_ context.Context, args ...string) ([]byte, error) {
	switch args[0] + " " + args[1] {
	case "run list":
		f.listings++
		if f.listErr != nil {
			return nil, f.listErr
		}
		return json.Marshal(f.runs)
	case "run view":
		id := args[2]
		for _, r := range f.runs {
			if fmt.Sprint(r.ID) == id {
				return []byte(f.logs[r.ID][r.Attempt-1]), nil
			}
		}
		return nil, fmt.Errorf("no run %s", id)
	case "run rerun":
		f.reruns = append(f.reruns, strings.Join(args[2:], " "))
		f.onRerun(f)
		return nil, nil
	}
	f.t.Fatalf("unexpected gh %v", args)
	return nil, nil
}

func gate(f *fakeGH) *Gate {
	return &Gate{GH: f, Repo: "o/r", MaxPolls: 5, Sleep: func(context.Context, time.Duration) {}}
}

// rerunTo returns a hook that makes attempt 2 complete with conclusion.
func rerunTo(conclusion string) func(*fakeGH) {
	return func(f *fakeGH) {
		f.runs[0].Attempt, f.runs[0].Status, f.runs[0].Conclusion = 2, "completed", conclusion
	}
}

const failLog = "--- FAIL: TestRenameClosedTask (0.01s)\nFAIL\n"

func TestGreenNoRerun(t *testing.T) {
	f := &fakeGH{t: t, runs: []Run{{7, "ci", "completed", "success", 1}}}
	r, err := gate(f).Check(context.Background(), "abc")
	if err != nil || r.Verdict != Green || r.Rerun || len(f.reruns) != 0 {
		t.Fatalf("%+v %v reruns=%v", r, err, f.reruns)
	}
}

func TestStaleRedIsRerunNotReused(t *testing.T) {
	// A completed-red head (as on a re-queued request) must be rerun; the
	// fresh attempt is green, so the merge proceeds and the flake is recorded.
	f := &fakeGH{
		t: t, runs: []Run{{7, "ci", "completed", "failure", 1}},
		logs: map[int64][]string{7: {failLog, ""}}, onRerun: rerunTo("success"),
	}
	r, err := gate(f).Check(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != Green || !r.Rerun || !reflect.DeepEqual(r.Flaked, []string{"TestRenameClosedTask"}) {
		t.Fatalf("%+v", r)
	}
	if !reflect.DeepEqual(f.reruns, []string{"7 -R o/r --failed"}) {
		t.Fatalf("reruns %v", f.reruns)
	}
}

func TestRedBothTimesReportsTests(t *testing.T) {
	f := &fakeGH{
		t: t, runs: []Run{{7, "ci", "completed", "failure", 1}},
		logs: map[int64][]string{7: {failLog + "--- FAIL: TestOther\n", failLog}}, onRerun: rerunTo("failure"),
	}
	r, err := gate(f).Check(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if r.Verdict != Red || !r.Rerun || len(f.reruns) != 1 {
		t.Fatalf("%+v reruns %v", r, f.reruns)
	}
	if !reflect.DeepEqual(r.FailedBoth, []string{"TestRenameClosedTask"}) ||
		!reflect.DeepEqual(r.FirstFailed, []string{"TestOther", "TestRenameClosedTask"}) ||
		!reflect.DeepEqual(r.SecondFailed, []string{"TestRenameClosedTask"}) {
		t.Fatalf("%+v", r)
	}
	if !strings.Contains(r.Summary, "failed both times: [TestRenameClosedTask]") {
		t.Fatal(r.Summary)
	}
}

func TestWaitsForInProgressThenReruns(t *testing.T) {
	f := &fakeGH{
		t: t, runs: []Run{{7, "ci", "in_progress", "", 1}},
		logs: map[int64][]string{7: {"build broke\n", ""}}, onRerun: rerunTo("success"),
	}
	g := gate(f)
	polls := 0
	g.Sleep = func(context.Context, time.Duration) {
		if polls++; polls == 2 {
			f.runs[0].Status, f.runs[0].Conclusion = "completed", "failure"
		}
	}
	r, err := g.Check(context.Background(), "abc")
	if err != nil || r.Verdict != Green || !r.Rerun || !reflect.DeepEqual(r.Flaked, []string{"run:ci"}) {
		t.Fatalf("%+v %v", r, err)
	}
}

func TestRerunWaitIgnoresOldAttempt(t *testing.T) {
	// After rerun the old attempt-1 failure is still listed for a poll; the
	// gate must keep waiting rather than read it as the verdict.
	f := &fakeGH{
		t: t, runs: []Run{{7, "ci", "completed", "failure", 1}},
		logs: map[int64][]string{7: {failLog, ""}},
	}
	f.onRerun = func(*fakeGH) {} // GitHub has not yet created attempt 2
	g := gate(f)
	polls := 0
	g.Sleep = func(context.Context, time.Duration) {
		if polls++; polls == 2 {
			f.runs[0].Attempt, f.runs[0].Conclusion = 2, "success"
		}
	}
	r, err := g.Check(context.Background(), "abc")
	if err != nil || r.Verdict != Green || polls < 2 {
		t.Fatalf("%+v %v polls=%d", r, err, polls)
	}
}

func TestNoChecksAndTimeout(t *testing.T) {
	f := &fakeGH{t: t}
	if r, _ := gate(f).Check(context.Background(), "abc"); r.Verdict != NoChecks {
		t.Fatalf("%+v", r)
	}
	f = &fakeGH{t: t, runs: []Run{{7, "ci", "in_progress", "", 1}}}
	if r, _ := gate(f).Check(context.Background(), "abc"); r.Verdict != Timeout {
		t.Fatalf("%+v", r)
	}
}

func TestGHErrorPropagates(t *testing.T) {
	f := &fakeGH{t: t, listErr: fmt.Errorf("boom")}
	if _, err := gate(f).Check(context.Background(), "abc"); err == nil {
		t.Fatal("want error")
	}
}
