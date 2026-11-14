package auth

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid JSON: "+err.Error(), false)
		return err
	}
	return nil
}

func isUnauthorized(err error) bool {
	var ue *UnauthorizedError
	return errors.As(err, &ue)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("auth: encode response: %v", err)
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
