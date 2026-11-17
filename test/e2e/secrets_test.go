package e2e_test

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

const (
	e2eTokenValue = "E2E-GH-TOKEN-5c0a91d7b3"
	e2eAWSValue   = "E2E-AWS-KEY-e44f02aa18"
)

// logCapture swaps the std logger's output for a buffer so the no-leak
// assertions can scan every line the factory, foreman and harness logged.
type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// leakFree fails loudly when any secret value appears in data.
func leakFree(t *testing.T, where string, data []byte, values ...string) {
	t.Helper()
	for _, v := range values {
		if bytes.Contains(data, []byte(v)) {
			t.Errorf("SECRET LEAK: value %q found in %s", v, where)
		}
	}
}

// foremanBearer reads the foreman's paired factory bearer from paired.json.
func foremanBearer(t *testing.T, s *stack) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.foremanDataDir, "paired.json"))
	if err != nil {
		t.Fatalf("read paired.json: %v", err)
	}
	var rec struct {
		Bearer string `json:"bearer"`
	}
	if err := json.Unmarshal(b, &rec); err != nil || rec.Bearer == "" {
		t.Fatalf("paired.json bearer: %v", err)
	}
	return rec.Bearer
}

// TestSecretsE2E: job secrets end to end on the real factory + foreman
// stack (spec §13.2). Subtests share the boot and run in order.
func TestSecretsE2E(t *testing.T) {
	logs := &logCapture{}
	log.SetOutput(logs)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s := boot(t)
	allValues := []string{e2eTokenValue, e2eAWSValue}

	// Store two secrets through the real HTTP surface.
	for name, in := range map[string]map[string]string{
		"github-push": {"value": e2eTokenValue, "env_var": "GITHUB_TOKEN"},
		"aws-key":     {"value": e2eAWSValue},
	} {
		if st := s.do("POST", "/v1/secrets/"+name, "", in, nil); st != 200 {
			t.Fatalf("POST /v1/secrets/%s = %d", name, st)
		}
	}

	// Fake runner: dumps its environment OUTSIDE both data dirs (so the
	// raw-file leak scan covers only what the system itself persisted)
	// and echoes the token on stdout, which becomes the fallback summary.
	dumpDir := t.TempDir()
	writeRunner := func(dump string) {
		script := "#!/bin/sh\nenv > '" + dump + "'\nprintf 'ran; token=%s' \"$GITHUB_TOKEN\"\n"
		if err := os.WriteFile(s.fakeOpencode, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dumpPath := filepath.Join(dumpDir, "env-with-secrets.txt")
	writeRunner(dumpPath)

	var jobID string
	t.Run("create validation fails loudly at create", func(t *testing.T) {
		// MCP path: unknown names error and create nothing.
		status, out := s.rpc(s.factoryMCP(), s.factory.PlannerToken(), "tools/call", map[string]any{
			"name":      "create_job",
			"arguments": map[string]any{"task": "x", "secrets": []string{"ghost-b", "github-push", "ghost-a"}},
		})
		res, _ := out["result"].(map[string]any)
		raw, _ := json.Marshal(res)
		if status != 200 || res == nil || res["isError"] != true || !strings.Contains(string(raw), "ghost-a") || !strings.Contains(string(raw), "ghost-b") {
			t.Fatalf("create_job unknown secrets: status %d result %s, want tool error naming ghost-a, ghost-b", status, raw)
		}
		// UI path: same, as a 400.
		var e struct {
			Error struct{ Code, Message string }
		}
		if st := s.do("POST", "/v1/jobs", "", map[string]any{"task": "x", "secrets": []string{"ghost-a"}}, &e); st != 400 || !strings.Contains(e.Error.Message, "ghost-a") {
			t.Fatalf("POST /v1/jobs unknown secrets = %d %+v, want 400 naming ghost-a", st, e)
		}
		var list []jobs.Job
		s.do("GET", "/v1/jobs", "", nil, &list)
		if len(list) != 0 {
			t.Fatalf("%d jobs exist after rejected creates, want 0", len(list))
		}
	})

	t.Run("env injection", func(t *testing.T) {
		sc := s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "create_job", map[string]any{
			"task": "e2e secrets: use the credentials", "secrets": []string{"github-push", "aws-key"},
		})
		jobID, _ = sc["job_id"].(string)
		var job jobs.Job
		waitFor(t, "secret job done", 10*time.Second, func() bool {
			_ = s.do("GET", "/v1/jobs/"+jobID, "", nil, &job)
			return job.State == jobs.StateDone
		})
		dump, err := os.ReadFile(dumpPath)
		if err != nil {
			t.Fatalf("runner env dump: %v", err)
		}
		for _, want := range []string{"GITHUB_TOKEN=" + e2eTokenValue, "SECRET_AWS_KEY=" + e2eAWSValue, "WORKER_TOKEN="} {
			if !strings.Contains(string(dump), want) {
				t.Errorf("child env missing %q", want)
			}
		}
	})

	t.Run("no leak", func(t *testing.T) {
		if jobID == "" {
			t.Fatal("injection scenario did not run")
		}
		// (1) Raw job.json bytes: names only.
		specBytes, err := os.ReadFile(filepath.Join(s.foremanDataDir, "jobs", jobID, "job.json"))
		if err != nil {
			t.Fatal(err)
		}
		leakFree(t, "job.json", specBytes, allValues...)
		var spec wire.JobSpec
		if err := json.Unmarshal(specBytes, &spec); err != nil || len(spec.Secrets) != 2 {
			t.Errorf("job.json spec.secrets = %v (%v), want the two NAMES", spec.Secrets, err)
		}

		// (2) Every byte the system persisted - inbox payloads (both
		// inbox.db files incl. WAL), jobs.db, auth.db, job dirs - except
		// the store itself.
		scan := func(root string) {
			_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
				if err != nil || d.IsDir() || filepath.Base(p) == "secrets.json" {
					return nil
				}
				b, rerr := os.ReadFile(p)
				if rerr == nil {
					leakFree(t, p, b, allValues...)
				}
				return nil
			})
		}
		scan(s.foremanDataDir)
		scan(s.factoryDataDir)

		// (3) Captured logs from every component.
		leakFree(t, "captured logs", []byte(logs.String()), allValues...)

		// (4) Job detail and list responses, raw.
		for _, p := range []string{"/v1/jobs/" + jobID, "/v1/jobs", "/v1/secrets"} {
			req, _ := http.NewRequest("GET", s.factorySrv.URL+p, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			_, _ = buf.ReadFrom(resp.Body)
			resp.Body.Close()
			leakFree(t, "GET "+p, buf.Bytes(), allValues...)
		}

		// (5) The echoing runner was scrubbed rather than posted.
		var job jobs.Job
		s.do("GET", "/v1/jobs/"+jobID, "", nil, &job)
		if !strings.Contains(string(job.Results), "[REDACTED]") {
			t.Errorf("result lacks redaction marker (runner echoed the token): %s", job.Results)
		}
	})

	t.Run("audit", func(t *testing.T) {
		var job jobs.Job
		s.do("GET", "/v1/jobs/"+jobID, "", nil, &job)
		if len(job.GrantedSecrets) != 2 || job.GrantedSecrets[0] != "aws-key" || job.GrantedSecrets[1] != "github-push" {
			t.Errorf("granted_secrets = %v, want [aws-key github-push]", job.GrantedSecrets)
		}
		g, err := s.factory.Jobs().GrantedSecrets(s.ctx, jobID)
		if err != nil || len(g) != 2 {
			t.Errorf("secret_grants rows = %v (%v), want 2", g, err)
		}
	})

	t.Run("handout is foreman-only", func(t *testing.T) {
		path := "/v1/jobs/" + jobID + "/secrets"
		beta := s.pairSynthetic(t, "beta")
		for name, tc := range map[string]struct {
			bearer string
			want   int
		}{
			"none":    {"", 401},
			"planner": {s.factory.PlannerToken(), 403},
			"foreign": {beta, 403},
		} {
			if st := s.do("GET", path, tc.bearer, nil, nil); st != tc.want {
				t.Errorf("%s: status %d, want %d", name, st, tc.want)
			}
		}
		// The owner is refused only because the job is terminal now.
		if st := s.do("GET", path, foremanBearer(t, s), nil, nil); st != http.StatusConflict {
			t.Errorf("owner on terminal job: %d, want 409", st)
		}
	})

	t.Run("no secrets means no injection", func(t *testing.T) {
		dump2 := filepath.Join(dumpDir, "env-plain.txt")
		writeRunner(dump2)
		sc := s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "create_job", map[string]any{"task": "e2e plain job"})
		id, _ := sc["job_id"].(string)
		waitFor(t, "plain job done", 10*time.Second, func() bool {
			var j jobs.Job
			_ = s.do("GET", "/v1/jobs/"+id, "", nil, &j)
			return j.State == jobs.StateDone
		})
		b, err := os.ReadFile(dump2)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "GITHUB_TOKEN=") || strings.Contains(string(b), e2eTokenValue) {
			t.Error("a job without secrets saw GITHUB_TOKEN")
		}
	})

	t.Run("fetch failure fails the job loudly", func(t *testing.T) {
		dump3 := filepath.Join(dumpDir, "env-never.txt")
		writeRunner(dump3)
		// Deterministic: the secret vanishes between dispatch and the
		// foreman's fetch (the spawner calls BuildHarnessConfig first).
		s.mu.Lock()
		s.onBuild = func(wire.JobSpec) {
			s.do("DELETE", "/v1/secrets/aws-key", "", nil, nil)
		}
		s.mu.Unlock()
		sc := s.callTool(s.factoryMCP(), s.factory.PlannerToken(), "create_job", map[string]any{
			"task": "e2e secrets: will lose its credential", "secrets": []string{"aws-key"},
		})
		id, _ := sc["job_id"].(string)
		var job jobs.Job
		waitFor(t, "failed result recorded", 10*time.Second, func() bool {
			_ = s.do("GET", "/v1/jobs/"+id, "", nil, &job)
			return strings.Contains(string(job.Results), `"failed"`)
		})
		res := string(job.Results)
		if !strings.Contains(res, "could not fetch secrets [aws-key]") {
			t.Errorf("failure lacks a clear error naming the secret: %s", res)
		}
		leakFree(t, "failed job results", []byte(res), allValues...)
		time.Sleep(300 * time.Millisecond)
		if _, err := os.Stat(dump3); err == nil {
			t.Error("the runner executed although its credentials could not be fetched")
		}
		leakFree(t, "captured logs (after failure)", []byte(logs.String()), allValues...)
	})
}
