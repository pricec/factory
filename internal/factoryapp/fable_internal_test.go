package factoryapp

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/models"
)

func TestFableHeldUntilApproved(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	setUsage(a, clk, 10) // GREEN: only the Fable gate holds

	code, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "hard", "model": models.Fable})
	if code != http.StatusCreated || out["state"] != jobs.StateHeld || out["held_reason"] != "fable: awaiting operator approval" {
		t.Fatalf("create: %d %v", code, out)
	}
	id := out["job_id"].(string)

	// The releaser never releases an unapproved Fable job, even in GREEN.
	if n, err := a.ReleasePass(ctx); err != nil || n != 0 {
		t.Fatalf("released unapproved: %d %v", n, err)
	}
	if j := mustJob(t, a, id); j.State != jobs.StateHeld {
		t.Fatalf("state %s", j.State)
	}

	// A non-Fable job is not gated.
	if _, o := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "easy", "model": "claude-opus-5-5"}); o["state"] == jobs.StateHeld {
		t.Fatalf("non-fable held: %v", o)
	}

	// Approval releases it (usage GREEN).
	code, ap := doJSON(t, a, "POST", "/v1/jobs/"+id+"/approve", nil)
	if code != http.StatusOK || ap["ok"] != true {
		t.Fatalf("approve: %d %v", code, ap)
	}
	if j := mustJob(t, a, id); j.State == jobs.StateHeld {
		t.Fatalf("still held after approval: %+v", j)
	}
	// Second approval: no longer held.
	if code, _ := doJSON(t, a, "POST", "/v1/jobs/"+id+"/approve", nil); code != http.StatusConflict {
		t.Fatalf("re-approve: %d", code)
	}
}

func TestFableApprovalDoesNotBypassThrottle(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	setUsage(a, clk, 96) // RED

	_, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "hard", "model": models.Fable})
	id := out["job_id"].(string)
	if code, _ := doJSON(t, a, "POST", "/v1/jobs/"+id+"/approve", nil); code != http.StatusOK {
		t.Fatalf("approve: %d", code)
	}
	j := mustJob(t, a, id)
	if j.State != jobs.StateHeld || !strings.HasPrefix(j.HeldReason, "usage RED") {
		t.Fatalf("approved job under RED: state %s reason %q", j.State, j.HeldReason)
	}
	// Usage recovers: only then does it release.
	setUsage(a, clk, 10)
	if n, _ := a.ReleasePass(ctx); n != 0 { // hysteresis keeps RED
		t.Fatalf("released during hysteresis: %d", n)
	}
	clk.Add(10 * time.Minute)
	setUsage(a, clk, 10)
	if n, err := a.ReleasePass(ctx); err != nil || n != 1 {
		t.Fatalf("release after recovery: %d %v", n, err)
	}
}

func TestFableConcurrencyCap(t *testing.T) {
	ctx := context.Background()
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	setUsage(a, clk, 10)

	_, o1 := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "one", "model": models.Fable})
	id1 := o1["job_id"].(string)
	doJSON(t, a, "POST", "/v1/jobs/"+id1+"/approve", nil)
	if err := a.jobs.Dispatch(ctx, id1, "foreman:x"); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	_, o2 := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "two", "model": models.Fable})
	id2 := o2["job_id"].(string)
	doJSON(t, a, "POST", "/v1/jobs/"+id2+"/approve", nil)
	if j := mustJob(t, a, id2); j.State != jobs.StateHeld || !strings.Contains(j.HeldReason, "max 1 concurrent") {
		t.Fatalf("cap not enforced: %s %q", j.State, j.HeldReason)
	}
}

func TestApproveNonFableRejected(t *testing.T) {
	clk := &gateClock{t: time.Date(2026, 11, 16, 21, 0, 0, 0, time.UTC)}
	a := newGateApp(t, t.TempDir(), clk)
	defer a.Close()
	setUsage(a, clk, 99)
	_, out := doJSON(t, a, "POST", "/v1/jobs", map[string]any{"task": "x"})
	if code, _ := doJSON(t, a, "POST", "/v1/jobs/"+out["job_id"].(string)+"/approve", nil); code != http.StatusConflict {
		t.Fatalf("code %d", code)
	}
	if code, _ := doJSON(t, a, "POST", "/v1/jobs/job_nope/approve", nil); code != http.StatusNotFound {
		t.Fatalf("code %d", code)
	}
}
