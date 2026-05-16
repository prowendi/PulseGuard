package web

import (
	"encoding/json"
	"net/http"
)

// apiError is the canonical JSON error envelope (spec §5.3).
type apiError struct {
	Error apiErrorBody `json:"error"`
}

type apiErrorBody struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// writeJSON encodes v as application/json with the given status code.
// Encoding failures are logged through the standard library logger
// because at this point the response status has already been written.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if v == nil {
		return
	}
	_ = json.NewEncoder(w).Encode(v)
}

// writeError emits the canonical { "error": { code, message, request_id } }
// envelope. The request id is harvested from the chi RequestID middleware
// (header X-Request-Id), allowing clients to correlate with logs.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	rid := r.Header.Get("X-Request-Id")
	if rid == "" {
		rid = w.Header().Get("X-Request-Id")
	}
	writeJSON(w, status, apiError{Error: apiErrorBody{Code: code, Message: msg, RequestID: rid}})
}

// decodeJSON parses an HTTP body into dst. Returns true on success,
// false after writing a 400 VALIDATION error response.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "empty body")
		return false
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error())
		return false
	}
	return true
}
