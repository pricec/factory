package wire

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"
)

func raw(s string) json.RawMessage { return json.RawMessage(s) }

func assertRoundTrip(t *testing.T, in any) {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := reflect.New(reflect.TypeOf(in))
	if err := json.Unmarshal(data, out.Interface()); err != nil {
		t.Fatalf("unmarshal(%s): %v", data, err)
	}
	if got := out.Elem().Interface(); !reflect.DeepEqual(got, in) {
		t.Errorf("round trip mismatch\ndata:  %s\ngot:  %+v\nwant: %+v", data, got, in)
	}
}

func TestNewMessageID(t *testing.T) {
	t.Parallel()

	id := NewMessageID()
	if !strings.HasPrefix(id, MessageIDPrefix) {
		t.Errorf("id %q: want %q prefix", id, MessageIDPrefix)
	}
	if _, err := ulid.Parse(strings.TrimPrefix(id, MessageIDPrefix)); err != nil {
		t.Errorf("id %q: not a ULID: %v", id, err)
	}
	if id == NewMessageID() {
		t.Errorf("ids not unique: %q twice", id)
	}
}

func TestNewJobID(t *testing.T) {
	t.Parallel()

	id := NewJobID()
	if !strings.HasPrefix(id, JobIDPrefix) {
		t.Errorf("id %q: want %q prefix", id, JobIDPrefix)
	}
	if _, err := ulid.Parse(strings.TrimPrefix(id, JobIDPrefix)); err != nil {
		t.Errorf("id %q: not a ULID: %v", id, err)
	}
	if id == NewJobID() {
		t.Errorf("ids not unique: %q twice", id)
	}
}

func TestJobScoped(t *testing.T) {
	t.Parallel()

	cases := []struct {
		msgType string
		want    bool
	}{
		{"job.request", true},
		{"job.result", true},
		{"job.event", true},
		{"job.question", true},
		{"job.answer", true},
		{"job.", true},
		{"brain.wake", false},
		{"chat.message", false},
		{"job", false},
		{"jobs.result", false},
		{"", false},
	}
	for _, tc := range cases {
		if got := JobScoped(tc.msgType); got != tc.want {
			t.Errorf("JobScoped(%q) = %v, want %v", tc.msgType, got, tc.want)
		}
	}
}

func TestEnvelopeValidate(t *testing.T) {
	t.Parallel()

	base := func() MessageEnvelope {
		return MessageEnvelope{
			ID:      NewMessageID(),
			Type:    "job.result",
			JobID:   NewJobID(),
			From:    "foreman:alpha",
			Payload: raw(`{"status":"done"}`),
			TS:      "2026-11-14T05:00:00Z",
		}
	}
	cases := []struct {
		name    string
		mutate  func(*MessageEnvelope)
		wantErr bool
	}{
		{"ok full envelope", nil, false},
		{"ok brain.wake without job_id", func(e *MessageEnvelope) {
			e.Type = "brain.wake"
			e.JobID = ""
		}, false},
		{"ok unmatched type with job_id", func(e *MessageEnvelope) {
			e.Type = "chat.message"
		}, false},
		{"ok null payload", func(e *MessageEnvelope) { e.Payload = raw(`null`) }, false},
		{"ok empty type but job-scoped absent", func(e *MessageEnvelope) {
			e.Type = "brain.wake"
			e.JobID = ""
			e.Payload = nil
		}, false},
		{"ok id and ts empty (client-side)", func(e *MessageEnvelope) {
			e.ID = ""
			e.TS = ""
		}, false},
		{"reject empty type", func(e *MessageEnvelope) { e.Type = "" }, true},
		{"reject empty from", func(e *MessageEnvelope) { e.From = "" }, true},
		{"reject job.result without job_id", func(e *MessageEnvelope) { e.JobID = "" }, true},
		{"reject job.request without job_id", func(e *MessageEnvelope) {
			e.Type = "job.request"
			e.JobID = ""
		}, true},
		{"reject job_id without job_ prefix", func(e *MessageEnvelope) {
			e.Type = "chat.message"
			e.JobID = "123"
		}, true},
		{"reject bare job_ job_id", func(e *MessageEnvelope) { e.JobID = "job_" }, true},
		{"reject invalid payload", func(e *MessageEnvelope) { e.Payload = raw(`{"oops`) }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := base()
			if tc.mutate != nil {
				tc.mutate(&e)
			}
			err := e.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	t.Parallel()

	for i, e := range []MessageEnvelope{
		{
			ID:      NewMessageID(),
			Type:    "job.result",
			JobID:   NewJobID(),
			From:    "foreman:alpha",
			Payload: raw(`{"status":"done","usage":{"cost_usd":0.5}}`),
			TS:      "2026-11-14T05:00:00Z",
		},
		{Type: "brain.wake", From: "foreman:alpha"},
		{Type: "job.event", JobID: "job_01", From: "foreman:beta", Payload: raw(`null`)},
		{Type: "chat.message", From: "human:operator", ThreadID: "task_01ABC"},
	} {
		t.Run(fmt.Sprintf("case %d", i), func(t *testing.T) {
			t.Parallel()
			assertRoundTrip(t, e)
		})
	}
}

func validSpec() JobSpec {
	return NewJobSpec("prompt for the worker", "factory")
}

func TestSpecValidate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		mutate  func(*JobSpec)
		wantErr bool
	}{
		{"ok constructor defaults", nil, false},
		{"ok with requires", func(s *JobSpec) { s.Requires = []string{"gpu", "docker"} }, false},
		{"ok empty tier means the cheap default", func(s *JobSpec) { s.Tier = "" }, false},
		{"ok flash tier", func(s *JobSpec) { s.Tier = TierFlash }, false},
		{"ok explicit default tier", func(s *JobSpec) { s.Tier = TierDefault }, false},
		{"ok hardest tier", func(s *JobSpec) { s.Tier = TierHardest }, false},
		{"reject unknown tier", func(s *JobSpec) { s.Tier = "turbo" }, true},
		{"ok with payload and schema", func(s *JobSpec) {
			s.Payload = raw(`{"input":[1,2]}`)
			s.ResultSchema = raw(`{"type":"object"}`)
		}, false},
		{"ok empty schema", func(s *JobSpec) { s.ResultSchema = raw(`{}`) }, false},
		{"ok without created_by/created_at", func(s *JobSpec) {
			s.CreatedBy = ""
			s.CreatedAt = ""
		}, false},
		{"ok priority 1", func(s *JobSpec) { s.Priority = MinPriority }, false},
		{"ok priority 9", func(s *JobSpec) { s.Priority = MaxPriority }, false},
		{"reject empty id", func(s *JobSpec) { s.ID = "" }, true},
		{"reject msg_ prefix on job id", func(s *JobSpec) { s.ID = NewMessageID() }, true},
		{"reject bare job_ id", func(s *JobSpec) { s.ID = "job_" }, true},
		{"reject empty task", func(s *JobSpec) { s.Task = "" }, true},
		{"reject zero timeout", func(s *JobSpec) { s.TimeoutS = 0 }, true},
		{"reject negative timeout", func(s *JobSpec) { s.TimeoutS = -1 }, true},
		{"reject priority 0", func(s *JobSpec) { s.Priority = 0 }, true},
		{"reject priority 10", func(s *JobSpec) { s.Priority = 10 }, true},
		{"reject empty requires entry", func(s *JobSpec) { s.Requires = []string{""} }, true},
		{"reject empty requires entry among valid", func(s *JobSpec) {
			s.Requires = []string{"gpu", ""}
		}, true},
		{"reject zero budget prompt bytes", func(s *JobSpec) { s.Budget.MaxPromptBytes = 0 }, true},
		{"reject negative budget cost", func(s *JobSpec) { s.Budget.MaxCostUSD = -1 }, true},
		{"reject invalid payload", func(s *JobSpec) { s.Payload = raw(`{nope`) }, true},
		{"reject schema outside subset", func(s *JobSpec) {
			s.ResultSchema = raw(`{"type":"string","enum":["a"]}`)
		}, true},
		{"ok parent_job_id (worker sub-spawning)", func(s *JobSpec) {
			s.ParentJobID = NewJobID()
		}, false},
		{"reject unprefixed parent_job_id", func(s *JobSpec) { s.ParentJobID = "abc" }, true},
		{"reject self parent_job_id", func(s *JobSpec) { s.ParentJobID = s.ID }, true},
		{"ok foreman pin (full form, §6.7)", func(s *JobSpec) { s.Foreman = "foreman:server" }, false},
		{"ok foreman pin (bare form pre-normalization)", func(s *JobSpec) { s.Foreman = "server" }, false},
		{"reject whitespace-only foreman pin", func(s *JobSpec) { s.Foreman = "   " }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := validSpec()
			if tc.mutate != nil {
				tc.mutate(&s)
			}
			err := s.Validate()
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSpecRoundTrip(t *testing.T) {
	t.Parallel()

	for i, s := range []JobSpec{
		{
			ID:           NewJobID(),
			Task:         "prompt for the worker",
			Payload:      raw(`{"repo":"github.com/pricec/factory"}`),
			ResultSchema: raw(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}}}`),
			Requires:     []string{"gpu", "linux"},
			TimeoutS:     120,
			Priority:     7,
			Budget:       Budget{MaxPromptBytes: 1024, MaxOutputTokens: 512, MaxCostUSD: 1.25},
			CreatedBy:    "factory",
			CreatedAt:    "2026-11-14T05:00:00Z",
		},
		{
			ID:       "job_01ARZ3NDEKTSV4RRFFQ69G5FAV",
			Task:     "bare",
			TimeoutS: DefaultTimeoutS,
			Priority: DefaultPriority,
			Budget:   defaultBudget(),
		},
	} {
		t.Run(fmt.Sprintf("case %d", i), func(t *testing.T) {
			t.Parallel()
			assertRoundTrip(t, s)
		})
	}
}

func TestNewJobSpecDefaults(t *testing.T) {
	t.Parallel()

	s := NewJobSpec("prompt for the worker", "factory")
	if !strings.HasPrefix(s.ID, JobIDPrefix) || len(s.ID) == len(JobIDPrefix) {
		t.Errorf("ID = %q, want job_-prefixed id", s.ID)
	}
	if s.TimeoutS != DefaultTimeoutS {
		t.Errorf("TimeoutS = %d, want %d", s.TimeoutS, DefaultTimeoutS)
	}
	if s.Priority != DefaultPriority {
		t.Errorf("Priority = %d, want %d", s.Priority, DefaultPriority)
	}
	if s.Budget != defaultBudget() {
		t.Errorf("Budget = %+v, want %+v", s.Budget, defaultBudget())
	}
	if s.CreatedBy != "factory" {
		t.Errorf("CreatedBy = %q, want %q", s.CreatedBy, "factory")
	}
	if _, err := time.Parse(time.RFC3339, s.CreatedAt); err != nil {
		t.Errorf("CreatedAt = %q: not RFC3339: %v", s.CreatedAt, err)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestNormalize(t *testing.T) {
	t.Parallel()

	t.Run("fills defaults on decoded spec", func(t *testing.T) {
		t.Parallel()
		var s JobSpec
		if err := json.Unmarshal(raw(`{"id":"job_01","task":"t"}`), &s); err != nil {
			t.Fatal(err)
		}
		s.Normalize()
		if s.TimeoutS != DefaultTimeoutS || s.Priority != DefaultPriority || s.Budget != defaultBudget() {
			t.Errorf("Normalize() = %+v, want defaults filled", s)
		}
		if s.ID != "job_01" || s.CreatedAt != "" {
			t.Errorf("Normalize() must not assign id or created_at, got %+v", s)
		}
		if err := s.Validate(); err != nil {
			t.Errorf("Validate() after Normalize() = %v, want nil", err)
		}
	})

	t.Run("keeps set values, fills partial budget", func(t *testing.T) {
		t.Parallel()
		var s JobSpec
		if err := json.Unmarshal(raw(`{"id":"job_01","task":"t","timeout_s":60,"priority":9,"budget":{"max_prompt_bytes":1024}}`), &s); err != nil {
			t.Fatal(err)
		}
		s.Normalize()
		if s.TimeoutS != 60 || s.Priority != 9 {
			t.Errorf("Normalize() = %+v, want set values kept", s)
		}
		want := Budget{MaxPromptBytes: 1024, MaxOutputTokens: DefaultMaxOutputTokens, MaxCostUSD: DefaultMaxCostUSD}
		if s.Budget != want {
			t.Errorf("Budget = %+v, want %+v", s.Budget, want)
		}
	})
}

func TestValidateTemplate(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		spec    JobSpec
		wantErr bool
	}{
		{"ok missing id and created_at", JobSpec{Task: "run nightly"}, false},
		{"ok with id", JobSpec{ID: "job_01", Task: "run"}, false},
		{"ok zero fields get defaults", JobSpec{Task: "run", Priority: 0, TimeoutS: 0}, false},
		{"ok custom values", JobSpec{Task: "run", TimeoutS: 60, Priority: 9}, false},
		{"reject empty task", JobSpec{}, true},
		{"reject priority out of range", JobSpec{Task: "run", Priority: 10}, true},
		{"reject empty requires entry", JobSpec{Task: "run", Requires: []string{""}}, true},
		{"reject bad id prefix", JobSpec{ID: "msg_01", Task: "run"}, true},
		{"reject schema outside subset", JobSpec{Task: "run", ResultSchema: raw(`{"minimum":1}`)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := tc.spec
			err := ValidateTemplate(&s)
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateTemplate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}

	t.Run("applies defaults", func(t *testing.T) {
		t.Parallel()
		s := JobSpec{Task: "run"}
		if err := ValidateTemplate(&s); err != nil {
			t.Fatalf("ValidateTemplate() = %v, want nil", err)
		}
		if s.TimeoutS != DefaultTimeoutS || s.Priority != DefaultPriority || s.Budget != defaultBudget() {
			t.Errorf("after ValidateTemplate() spec = %+v, want defaults applied", s)
		}
		if s.ID != "" || s.CreatedAt != "" {
			t.Errorf("ValidateTemplate() must not assign id or created_at, got %+v", s)
		}
	})

	t.Run("full spec requires id", func(t *testing.T) {
		t.Parallel()
		s := JobSpec{Task: "run"}
		if err := ValidateTemplate(&s); err != nil {
			t.Fatalf("ValidateTemplate() = %v, want nil", err)
		}
		if err := s.Validate(); err == nil {
			t.Error("Validate() on id-less spec = nil, want error")
		}
	})
}

func doneResult() JobResult {
	return JobResult{
		JobID:   NewJobID(),
		Status:  StatusDone,
		Summary: "did the thing",
		Result:  raw(`{"answer":42}`),
	}
}

func TestResultValidate(t *testing.T) {
	t.Parallel()

	objectSchema := raw(`{"type":"object","required":["answer"],"properties":{"answer":{"type":"integer"}}}`)
	cases := []struct {
		name    string
		mutate  func(*JobResult)
		schema  json.RawMessage
		wantErr bool
	}{
		{"ok done without schema", nil, nil, false},
		{"ok done with conforming result", nil, objectSchema, false},
		{"ok done with empty schema", func(r *JobResult) { r.Result = raw(`{"anything":[1]}`) }, raw(`{}`), false},
		{"ok done without result when schema absent", func(r *JobResult) { r.Result = nil }, nil, false},
		{"ok usage all zeros", func(r *JobResult) { r.Usage = &Usage{} }, nil, false},
		{"ok usage positive", func(r *JobResult) {
			r.Usage = &Usage{InputTokens: 100, OutputTokens: 50, CostUSD: 0.75}
		}, nil, false},
		{"ok failed kind validation", func(r *JobResult) {
			r.Status = StatusFailed
			r.Result = nil
			r.Error = &JobError{Kind: KindValidation, Message: "bad payload"}
		}, nil, false},
		{"ok failed kind timeout", func(r *JobResult) {
			r.Status = StatusFailed
			r.Result = nil
			r.Error = &JobError{Kind: KindTimeout, Message: "timed out"}
		}, nil, false},
		{"ok failed kind crash", func(r *JobResult) {
			r.Status = StatusFailed
			r.Result = nil
			r.Error = &JobError{Kind: KindCrash, Message: "exit 1"}
		}, nil, false},
		{"ok failed kind llm", func(r *JobResult) {
			r.Status = StatusFailed
			r.Result = nil
			r.Error = &JobError{Kind: KindLLM, Message: "model error"}
		}, nil, false},
		{"ok failed kind other", func(r *JobResult) {
			r.Status = StatusFailed
			r.Result = nil
			r.Error = &JobError{Kind: KindOther, Message: "mystery"}
		}, nil, false},
		{"ok failed kind superseded", func(r *JobResult) {
			r.Status = StatusFailed
			r.Result = nil
			r.Error = &JobError{Kind: KindSuperseded, Message: "duplicate of job_x"}
		}, nil, false},
		{"ok failed ignores result and schema", func(r *JobResult) {
			r.Status = StatusFailed
			r.Error = &JobError{Kind: KindCrash, Message: "exit 1"}
		}, objectSchema, false},
		{"reject empty summary", func(r *JobResult) { r.Summary = "" }, nil, true},
		{"reject summary too long", func(r *JobResult) {
			r.Summary = strings.Repeat("x", MaxSummaryChars+1)
		}, nil, true},
		{"reject empty status", func(r *JobResult) { r.Status = "" }, nil, true},
		{"reject unknown status", func(r *JobResult) { r.Status = "running" }, nil, true},
		{"reject done missing result with schema", func(r *JobResult) { r.Result = nil }, objectSchema, true},
		{"reject done result not conforming", func(r *JobResult) {
			r.Result = raw(`{"answer":"not an integer"}`)
		}, objectSchema, true},
		{"reject done result missing required", func(r *JobResult) { r.Result = raw(`{}`) }, objectSchema, true},
		{"reject failed without error", func(r *JobResult) { r.Status = StatusFailed }, nil, true},
		{"reject failed with unknown kind", func(r *JobResult) {
			r.Status = StatusFailed
			r.Error = &JobError{Kind: "fatal", Message: "boom"}
		}, nil, true},
		{"reject failed with case-mangled kind", func(r *JobResult) {
			r.Status = StatusFailed
			r.Error = &JobError{Kind: "Timeout", Message: "timed out"}
		}, nil, true},
		{"reject negative usage tokens", func(r *JobResult) {
			r.Usage = &Usage{InputTokens: -1}
		}, nil, true},
		{"reject negative usage cost", func(r *JobResult) {
			r.Usage = &Usage{CostUSD: -0.01}
		}, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := doneResult()
			if tc.mutate != nil {
				tc.mutate(&r)
			}
			err := r.Validate(tc.schema)
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestSummaryBounds(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		summary string
		wantErr bool
	}{
		{"0 chars rejects", "", true},
		{"1 char ok", "x", false},
		{"240 chars ok", strings.Repeat("x", 240), false},
		{"241 chars rejects", strings.Repeat("x", 241), true},
		{"240 multibyte chars ok", strings.Repeat("é", 240), false},
		{"241 multibyte chars rejects", strings.Repeat("é", 241), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := doneResult()
			r.Summary = tc.summary
			err := r.Validate(nil)
			if (err != nil) != tc.wantErr {
				t.Errorf("Validate() = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestResultRoundTrip(t *testing.T) {
	t.Parallel()

	for i, r := range []JobResult{
		{
			JobID:   NewJobID(),
			Status:  StatusDone,
			Summary: "did the thing",
			Result:  raw(`{"answer":[1,{"nested":true}]}`),
			Usage:   &Usage{InputTokens: 100, OutputTokens: 50, CostUSD: 0.75},
		},
		{
			JobID:   NewJobID(),
			Status:  StatusFailed,
			Summary: "crashed",
			Error:   &JobError{Kind: KindCrash, Message: "exit 1"},
		},
		{JobID: "job_01", Status: StatusDone, Summary: "ok"},
	} {
		t.Run(fmt.Sprintf("case %d", i), func(t *testing.T) {
			t.Parallel()
			assertRoundTrip(t, r)
		})
	}
}

// ThreadID is optional and omitted when empty, so envelopes without it keep
// the pre-thread wire bytes; old JSON (no thread_id) decodes to "".
func TestEnvelopeThreadIDBackwardCompatible(t *testing.T) {
	t.Parallel()
	b, err := json.Marshal(MessageEnvelope{ID: "msg_1", Type: "chat.message", From: "x", TS: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"id":"msg_1","type":"chat.message","job_id":"","from":"x","ts":"t"}`; string(b) != want {
		t.Errorf("marshal = %s, want %s", b, want)
	}
	var e MessageEnvelope
	if err := json.Unmarshal([]byte(`{"id":"msg_1","type":"t","job_id":"","from":"x","ts":"t"}`), &e); err != nil || e.ThreadID != "" {
		t.Errorf("legacy decode: %+v %v", e, err)
	}
	b, _ = json.Marshal(MessageEnvelope{Type: "t", From: "x", ThreadID: "task_9"})
	if !strings.Contains(string(b), `"thread_id":"task_9"`) {
		t.Errorf("thread_id missing: %s", b)
	}
}

func TestNormalizeTimeoutDefaults(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		tier     string
		timeout  int
		wantCeil int
	}{
		{"unset no tier", "", 0, 14400},
		{"flash", TierFlash, 0, 3600},
		{"default", TierDefault, 0, 7200},
		{"hardest", TierHardest, 0, 14400},
		{"explicit wins", TierFlash, 90, 90},
	} {
		s := JobSpec{Tier: tc.tier, TimeoutS: tc.timeout}
		s.Normalize()
		if s.TimeoutS != tc.wantCeil || s.IdleTimeoutS != 900 {
			t.Errorf("%s: timeout_s=%d idle=%d, want %d/900", tc.name, s.TimeoutS, s.IdleTimeoutS, tc.wantCeil)
		}
	}
	s := JobSpec{IdleTimeoutS: 60}
	s.Normalize()
	if s.IdleTimeoutS != 60 {
		t.Errorf("explicit idle_timeout_s overridden: %d", s.IdleTimeoutS)
	}
}

// TestDefaultMaxCostIs20: the default live spend cap is $20 (an
// API-equivalent estimate for subscription usage, not real money).
func TestDefaultMaxCostIs20(t *testing.T) {
	t.Parallel()
	if DefaultMaxCostUSD != 20 {
		t.Errorf("DefaultMaxCostUSD = %v, want 20", DefaultMaxCostUSD)
	}
	s := NewJobSpec("t", "factory")
	if s.Budget.MaxCostUSD != 20 {
		t.Errorf("NewJobSpec budget = %v, want 20", s.Budget.MaxCostUSD)
	}
	var z JobSpec
	z.Normalize()
	if z.Budget.MaxCostUSD != 20 {
		t.Errorf("Normalize budget = %v, want 20", z.Budget.MaxCostUSD)
	}
}

func TestLoopLimitOmittedWhenZero(t *testing.T) {
	t.Parallel()
	b, _ := json.Marshal(NewJobSpec("t", "factory"))
	if strings.Contains(string(b), "loop_limit") {
		t.Errorf("loop_limit serialized when zero: %s", b)
	}
	s := NewJobSpec("t", "factory")
	s.LoopLimit = -1
	if err := s.Validate(); err != nil {
		t.Errorf("negative loop_limit (disabled) rejected: %v", err)
	}
}
