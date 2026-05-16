package web

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/wendi/pulseguard/internal/domain"

	"github.com/go-chi/chi/v5"
)

// botTokenPattern is the canonical Telegram bot token format:
// `<bot_id>:<token>` where the second part is base64url-ish.
var botTokenPattern = regexp.MustCompile(`^\d+:[A-Za-z0-9_-]+$`)

// parsePathID extracts a positive int64 from a chi URL param, writing
// a 400/VALIDATION response on failure and returning ok=false.
func parsePathID(w http.ResponseWriter, r *http.Request, key string) (int64, bool) {
	raw := chi.URLParam(r, key)
	if raw == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "missing path id")
		return 0, false
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "invalid path id")
		return 0, false
	}
	return n, true
}

// writeRepoError maps common domain errors into the JSON error envelope.
// Use it from CRUD handlers so they don't repeat the switch.
func writeRepoError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, domain.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "not found")
	case errors.Is(err, domain.ErrValidation):
		writeError(w, r, http.StatusBadRequest, "VALIDATION", err.Error())
	case errors.Is(err, domain.ErrConflict):
		writeError(w, r, http.StatusConflict, "CONFLICT", "conflict")
	case errors.Is(err, domain.ErrForbidden):
		writeError(w, r, http.StatusForbidden, "FORBIDDEN", "forbidden")
	default:
		writeError(w, r, http.StatusInternalServerError, "INTERNAL", err.Error())
	}
}

// validateName enforces non-empty + length<=max.
func validateName(w http.ResponseWriter, r *http.Request, name string, max int) bool {
	if name == "" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "name is required")
		return false
	}
	if len(name) > max {
		writeError(w, r, http.StatusBadRequest, "VALIDATION", "name too long")
		return false
	}
	return true
}
