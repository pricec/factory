package factoryapp_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/jobs"
	"github.com/pricec/factory/internal/wire"
)

const (
	handoutVal1 = "HANDOUT-VALUE-1-a8c3e1"
	handoutVal2 = "HANDOUT-VALUE-2-77d0f4"
	handoutVal3 = "HANDOUT-VALUE-3-not-in-spec"
)

type handoutEnv struct {
	*testEnv
	alpha, beta string // paired foreman bearers
	jobID       string
}

// newHandoutEnv stores three secrets and creates a job dispatched to
// foreman:alpha whose spec names only two of them.
func newHandoutEnv(t *testing.T) *handoutEnv {
	t.Helper()
	e := &handoutEnv{testEnv: newEnv(t)}
	for _, s := range []struct{ name, val, env string }{
		{"github-push", handoutVal1, "GITHUB_TOKEN"},
		{"aws-key", handoutVal2, ""},
		{"unrelated", handoutVal3, "UNRELATED"},
	} {
		in := map[string]string{"value": s.val}
		if s.env != "" {
			in["env_var"] = s.env
		}
		if st := e.do("POST", "/v1/secrets/"+s.name, "", in, nil); st != 200 {
			t.Fatalf("put %s: %d", s.name, st)
		}
	}
	for id, dst := range map[string]*string{"alpha": &e.alpha, "beta": &e.beta} {
		tok, _, err := e.a.Auth().IssuePairingToken(e.ctx)
		if err != nil {
			t.Fatal(err)
		}
		if *dst, _, err = e.a.Auth().PairForeman(e.ctx, tok, id); err != nil {
			t.Fatal(err)
		}
	}
	spec := wire.NewJobSpec("use secrets", "test")
	spec.Secrets = []string{"github-push", "aws-key"}
	if err := e.a.Jobs().Create(e.ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := e.a.Jobs().Dispatch(e.ctx, spec.ID, "foreman:alpha"); err != nil {
		t.Fatal(err)
	}
	e.jobID = spec.ID
	return e
}

func (e *handoutEnv) raw(method, path, bearer string) (int, string) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.ts.URL+path, nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestJobSecretsHandoutAuth: only the paired foreman that owns the job gets
// values; every other credential is refused with no value in the body.
func TestJobSecretsHandoutAuth(t *testing.T) {
	e := newHandoutEnv(t)
	workerTok, err := e.a.Auth().MintWorkerToken(e.ctx, "foreman:alpha", e.jobID)
	if err != nil {
		t.Fatal(err)
	}
	path := "/v1/jobs/" + e.jobID + "/secrets"
	tests := []struct {
		name, path, bearer string
		status             int
	}{
		{"no bearer", path, "", 401},
		{"garbage bearer", path, "nope", 401},
		{"planner credential", path, e.a.PlannerToken(), 403},
		{"worker credential", path, workerTok, 403},
		{"foreign foreman", path, e.beta, 403},
		{"unknown job looks like foreign", "/v1/jobs/job_nope/secrets", e.alpha, 403},
		{"owning foreman", path, e.alpha, 200},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			status, body := e.raw("GET", tc.path, tc.bearer)
			if status != tc.status {
				t.Fatalf("status = %d, want %d; body %s", status, tc.status, body)
			}
			if tc.status != 200 {
				for _, v := range []string{handoutVal1, handoutVal2, handoutVal3} {
					if strings.Contains(body, v) {
						t.Fatalf("refusal body leaked a value: %s", body)
					}
				}
			}
		})
	}
}

// TestJobSecretsHandoutScopeAndEcho: the handout returns exactly the spec's
// names (never other store entries), and values appear on that endpoint
// ONLY - not in the job detail, job list, secrets list or error bodies.
func TestJobSecretsHandoutScopeAndEcho(t *testing.T) {
	e := newHandoutEnv(t)
	var out struct {
		Secrets []struct {
			Name   string `json:"name"`
			EnvVar string `json:"env_var"`
			Value  string `json:"value"`
		} `json:"secrets"`
	}
	status, body := e.raw("GET", "/v1/jobs/"+e.jobID+"/secrets", e.alpha)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, body)
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]string{}
	for _, s := range out.Secrets {
		got[s.Name] = [2]string{s.EnvVar, s.Value}
	}
	want := map[string][2]string{
		"github-push": {"GITHUB_TOKEN", handoutVal1},
		"aws-key":     {"SECRET_AWS_KEY", handoutVal2},
	}
	if len(got) != 2 || got["github-push"] != want["github-push"] || got["aws-key"] != want["aws-key"] {
		t.Fatalf("handout = %v, want %v (and nothing outside the spec)", got, want)
	}
	if strings.Contains(body, handoutVal3) || strings.Contains(body, "unrelated") {
		t.Errorf("handout leaked an out-of-spec secret: %s", body)
	}

	// Value echo ONLY on the handout: every other surface, raw bytes.
	for _, p := range []string{"/v1/jobs/" + e.jobID, "/v1/jobs", "/v1/secrets", "/v1/jobs/" + e.jobID + "/cancel"} {
		for _, m := range []string{"GET", "POST"} {
			_, b := e.raw(m, p, e.alpha)
			for _, v := range []string{handoutVal1, handoutVal2, handoutVal3} {
				if strings.Contains(b, v) {
					t.Errorf("%s %s leaked a secret value: %s", m, p, b)
				}
			}
		}
	}

	// Audit: names recorded, surfaced on the detail response.
	var job jobs.Job
	if st := e.do("GET", "/v1/jobs/"+e.jobID, "", nil, &job); st != 200 {
		t.Fatalf("detail status %d", st)
	}
	if len(job.GrantedSecrets) != 2 || job.GrantedSecrets[0] != "aws-key" || job.GrantedSecrets[1] != "github-push" {
		t.Errorf("granted_secrets = %v, want [aws-key github-push]", job.GrantedSecrets)
	}
	if len(job.Spec.Secrets) != 2 {
		t.Errorf("spec.secrets = %v, want the two names", job.Spec.Secrets)
	}
}

// TestJobSecretsHandoutRefusals: terminal jobs and since-deleted secrets
// are refused loudly, and a refused fetch writes no audit row.
func TestJobSecretsHandoutRefusals(t *testing.T) {
	e := newHandoutEnv(t)
	path := "/v1/jobs/" + e.jobID + "/secrets"

	if st := e.do("DELETE", "/v1/secrets/aws-key", "", nil, nil); st != 200 {
		t.Fatalf("delete: %d", st)
	}
	status, body := e.raw("GET", path, e.alpha)
	if status != 404 || !strings.Contains(body, "aws-key") || strings.Contains(body, handoutVal1) {
		t.Errorf("deleted secret: status %d body %s, want 404 naming aws-key, no values", status, body)
	}
	if g, _ := e.a.Jobs().GrantedSecrets(e.ctx, e.jobID); len(g) != 0 {
		t.Errorf("refused fetch wrote grants %v", g)
	}

	if _, err := e.a.Jobs().Cancel(e.ctx, e.jobID, jobs.StateCancelled, "test"); err != nil {
		t.Fatal(err)
	}
	status, body = e.raw("GET", path, e.alpha)
	if status != 409 || strings.Contains(body, handoutVal1) {
		t.Errorf("terminal job: status %d body %s, want 409 without values", status, body)
	}
}

// TestCreateJobSecretsValidation: UI create path - unknown names 400 with
// the names in the error; known names create and appear (names only).
func TestCreateJobSecretsValidation(t *testing.T) {
	e := newEnv(t)
	if st := e.do("POST", "/v1/secrets/known-one", "", map[string]string{"value": handoutVal1}, nil); st != 200 {
		t.Fatalf("put: %d", st)
	}
	tests := []struct {
		name    string
		secrets []string
		status  int
		errHas  []string
	}{
		{"no secrets", nil, 201, nil},
		{"known", []string{"known-one"}, 201, nil},
		{"unknown names listed", []string{"known-one", "ghost-b", "ghost-a"}, 400, []string{"unknown_secrets", "ghost-a", "ghost-b"}},
		{"bad name shape", []string{"Bad_Name"}, 400, []string{"secrets"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"task": "t", "secrets": tc.secrets})
			resp, err := http.Post(e.ts.URL+"/v1/jobs", "application/json", bytes.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.status, b)
			}
			for _, w := range tc.errHas {
				if !strings.Contains(string(b), w) {
					t.Errorf("error missing %q: %s", w, b)
				}
			}
			if strings.Contains(string(b), handoutVal1) {
				t.Errorf("response leaked a value: %s", b)
			}
		})
	}
	// Nothing was created for the rejected requests.
	var list []jobs.Job
	e.do("GET", "/v1/jobs", "", nil, &list)
	if len(list) != 2 {
		t.Errorf("%d jobs exist, want only the 2 valid creations", len(list))
	}
}
