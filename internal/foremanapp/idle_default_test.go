package foremanapp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
	"github.com/pricec/factory/internal/workerharness"
)

func TestDefaultIdleTimeoutS(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		cfg  Config
		want int
	}{
		{"megatron", Config{ID: "megatron"}, 3600},
		{"megatron prefixed", Config{ID: "foreman:megatron"}, 3600},
		{"other foreman", Config{ID: "server"}, 900},
		{"env override", Config{ID: "server", IdleTimeoutS: 1800}, 1800},
		{"env override beats table", Config{ID: "megatron", IdleTimeoutS: 1200}, 1200},
		{"env override capped", Config{ID: "server", IdleTimeoutS: 99999}, wire.MaxIdleTimeoutS},
	} {
		a := &App{cfg: tc.cfg}
		if got := a.defaultIdleTimeoutS(); got != tc.want {
			t.Errorf("%s: got %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestSpawnWorkerAppliesIdleDefault: the persisted job.json carries the
// foreman default only when the job did not set idle_timeout_s.
func TestSpawnWorkerAppliesIdleDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  int
		want int
	}{
		{"unset gets foreman default", 0, 3600},
		{"explicit wins", 7200, 7200},
	} {
		f := newFakeFactory(t)
		a := newDrainApp(t, f, buildOld)
		a.cfg.ID = "megatron"
		bin := filepath.Join(t.TempDir(), "opencode")
		if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf done\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		orig := a.BuildHarnessConfig
		a.BuildHarnessConfig = func(spec wire.JobSpec, bearer string) workerharness.Config {
			c := orig(spec, bearer)
			c.OpenCODEBin = bin
			c.Timeout = 10 * time.Second
			c.Post = func(context.Context, string, string, wire.JobResult) error { return nil }
			return c
		}
		spec := wire.NewJobSpec("x", "factory:main")
		spec.IdleTimeoutS = tc.set
		if err := a.SpawnWorker(context.Background(), spec); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(filepath.Join(a.dataDir, jobsDirName, spec.ID, "job.json"))
		if err != nil {
			t.Fatal(err)
		}
		var got wire.JobSpec
		if err := json.Unmarshal(data, &got); err != nil {
			t.Fatal(err)
		}
		if got.IdleTimeoutS != tc.want {
			t.Errorf("%s: job.json idle_timeout_s=%d, want %d", tc.name, got.IdleTimeoutS, tc.want)
		}
		waitUntil(t, 8*time.Second, func() bool { busy, _ := a.gate.busy(); return !busy })
	}
}
