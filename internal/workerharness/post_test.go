package workerharness

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/pricec/factory/internal/wire"
)

func testResult() wire.JobResult {
	return wire.JobResult{
		JobID:   wire.NewJobID(),
		Status:  wire.StatusFailed,
		Summary: "agent crashed early",
		Error:   &wire.JobError{Kind: wire.KindCrash, Message: "exit status 3"},
	}
}

func TestHTTPPost(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		status  int
		wantErr bool
		wantDup bool // want a statusError specifically
	}{
		{"201 created", http.StatusCreated, false, false},
		{"200 ok", http.StatusOK, false, false},
		{"409 duplicate is success", http.StatusConflict, false, false},
		{"400 bad request", http.StatusBadRequest, true, true},
		{"500 server error", http.StatusInternalServerError, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			res := testResult()
			var auth string
			var env wire.MessageEnvelope
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				auth = r.Header.Get("Authorization")
				body, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(body, &env); err != nil {
					t.Errorf("request body is not an envelope: %v", err)
				}
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()

			err := httpPost(context.Background(), srv.URL, "tok-abc", res)
			if (err != nil) != tc.wantErr {
				t.Fatalf("httpPost() = %v, wantErr %v", err, tc.wantErr)
			}
			var se *statusError
			if tc.wantDup && !errors.As(err, &se) {
				t.Errorf("httpPost() = %v, want *statusError", err)
			}
			if tc.wantErr {
				return
			}
			if auth != "Bearer tok-abc" {
				t.Errorf("Authorization = %q, want %q", auth, "Bearer tok-abc")
			}
			if env.Type != resultEnvelopeType {
				t.Errorf("envelope type = %q, want %q", env.Type, resultEnvelopeType)
			}
			if env.JobID != res.JobID || env.From != senderFrom(res.JobID) {
				t.Errorf("envelope = job %s from %s, want job %s from %s", env.JobID, env.From, res.JobID, senderFrom(res.JobID))
			}
			var payload wire.JobResult
			if err := json.Unmarshal(env.Payload, &payload); err != nil {
				t.Fatalf("payload is not a JobResult: %v", err)
			}
			if !reflect.DeepEqual(payload, res) {
				t.Errorf("payload = %+v, want %+v", payload, res)
			}
		})
	}
}

func TestPostResultRetry(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		fails      int
		definitive bool
		wantErr    bool
		wantCalls  int
	}{
		{"success first try", 0, false, false, 1},
		{"fails twice then succeeds", 2, false, false, 3},
		{"always fails", 99, false, true, maxPostAttempts},
		{"definitive rejection not retried", 1, true, true, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var mu sync.Mutex
			calls := 0
			cfg := Config{
				ResultURL:        "http://foreman.invalid/v1/inboxes/foreman:alpha/messages",
				Token:            "tok",
				PostRetryBackoff: time.Millisecond,
				Post: func(context.Context, string, string, wire.JobResult) error {
					mu.Lock()
					defer mu.Unlock()
					calls++
					if calls > tc.fails {
						return nil
					}
					if tc.definitive {
						return &statusError{code: http.StatusBadRequest}
					}
					return errors.New("connection refused")
				},
			}
			err := postResult(context.Background(), cfg, testResult())
			if (err != nil) != tc.wantErr {
				t.Errorf("postResult() = %v, wantErr %v", err, tc.wantErr)
			}
			mu.Lock()
			defer mu.Unlock()
			if calls != tc.wantCalls {
				t.Errorf("post attempts = %d, want %d", calls, tc.wantCalls)
			}
		})
	}
}
