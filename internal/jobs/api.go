package jobs

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
)

// APIHandler exposes the control-plane API (spec §6.9):
// GET /v1/jobs?state=&foreman=&limit=, GET /v1/jobs/{id}, GET /v1/foremen.
func (s *Store) APIHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/jobs", s.handleListJobs)
	mux.HandleFunc("GET /v1/jobs/{id}", s.handleGetJob)
	mux.HandleFunc("GET /v1/foremen", s.handleForemen)
	return mux
}

func (s *Store) handleListJobs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	if state != "" && !knownState(state) {
		writeError(w, http.StatusBadRequest, "bad_request", "unknown state filter: "+state, false)
		return
	}
	limit := 200
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 1000 {
			writeError(w, http.StatusBadRequest, "bad_request", "limit must be an integer in [1, 1000]", false)
			return
		}
		limit = n
	}
	jobs, err := s.List(r.Context(), state, q.Get("foreman"), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

func (s *Store) handleGetJob(w http.ResponseWriter, r *http.Request) {
	job, ok, err := s.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "no such job", false)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

func (s *Store) handleForemen(w http.ResponseWriter, r *http.Request) {
	foremen, err := s.Foremen(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), true)
		return
	}
	writeJSON(w, http.StatusOK, foremen)
}

func knownState(state string) bool {
	for _, s := range AllStates {
		if s == state {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("jobs: encode response: %v", err)
	}
}

// writeError emits the normative error model (spec §10).
func writeError(w http.ResponseWriter, status int, code, msg string, retryable bool) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{
			"code":      code,
			"message":   msg,
			"retryable": retryable,
		},
	})
}

func unmarshalJSON(data []byte, v any) error { return json.Unmarshal(data, v) }
