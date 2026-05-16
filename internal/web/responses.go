package web

import (
	"encoding/json"
	"errors"
	"net/http"

	chimid "github.com/go-chi/chi/v5/middleware"
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
// envelope. The request id is harvested from chi's RequestID context
// (chimid.GetReqID) so operators can correlate with the access log
// emitted by middleware.Logger.
func writeError(w http.ResponseWriter, r *http.Request, status int, code, msg string) {
	rid := chimid.GetReqID(r.Context())
	if rid == "" {
		// Fallback paths: a client-supplied request id, then any other
		// middleware that already pinned the header.
		rid = r.Header.Get("X-Request-Id")
		if rid == "" {
			rid = w.Header().Get("X-Request-Id")
		}
	}
	writeJSON(w, status, apiError{Error: apiErrorBody{Code: code, Message: msg, RequestID: rid}})
}

// writeInternal records the full error to the structured logger and
// returns an OPAQUE 500 to the client. The client only sees a generic
// message + the request_id so the operator can correlate the
// privileged log line. Implements security-report S-H2.
func writeInternal(w http.ResponseWriter, r *http.Request, deps Deps, label string, err error) {
	if deps.Logger != nil {
		deps.Logger.Error(label,
			"err", err.Error(),
			"path", r.URL.Path,
			"method", r.Method,
		)
	}
	writeError(w, r, http.StatusInternalServerError, "INTERNAL",
		"internal error; see request_id in response header")
}

// decodeJSON parses an HTTP body into dst. Returns true on success,
// false after writing a 400 VALIDATION error response.
//
// The reader is wrapped in http.MaxBytesReader(MaxRequestBodyBytes)
// before decode so a malicious client cannot OOM the process by
// streaming an unbounded JSON body.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "empty body")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxRequestBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, r, http.StatusRequestEntityTooLarge, "VALIDATION",
				"request body exceeds 1 MiB limit")
			return false
		}
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error())
		return false
	}
	return true
}

// MaxRequestBodyBytes is the default cap shared by every JSON-decoding
// authenticated endpoint (template preview, channel CRUD, bot CRUD,
// etc.). The push endpoint has its own cap (MaxPushBodyBytes) declared
// locally because it accepts arbitrary tenant-defined payloads.
const MaxRequestBodyBytes = 1 << 20
