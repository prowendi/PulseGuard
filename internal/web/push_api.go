package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/prowendi/PulseGuard/internal/domain"

	"github.com/go-chi/chi/v5"
)

// MaxPushBodyBytes caps the request body of POST /api/v1/push/{token}
// at 1 MiB. Above this the server returns 413 to prevent malicious
// clients from inflating SQLite (payload is stored as TEXT) or
// occupying connection memory while a JSON decoder drains them.
//
// Refs: security-report S-M4, code-review-report C-I5.
const MaxPushBodyBytes = 1 << 20

// installPushAPIRoutes wires POST /api/v1/push/{token}. The endpoint is
// intentionally OUTSIDE the auth/csrf middleware stack because its only
// bearer is the channel push_token in the URL path (spec §4.1).
func installPushAPIRoutes(r chi.Router, deps Deps) {
	r.Post("/push/{token}", apiPush(deps))
}

func apiPush(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := chi.URLParam(r, "token")
		if token == "" {
			writeError(w, r, http.StatusNotFound, "NOT_FOUND", "missing token")
			return
		}
		ch, err := deps.Channels.GetByPushToken(r.Context(), token)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				writeError(w, r, http.StatusNotFound, "NOT_FOUND", "unknown push token")
				return
			}
			writeInternal(w, r, deps, "push: channel lookup", err)
			return
		}
		if !ch.Enabled {
			writeError(w, r, http.StatusGone, "CHANNEL_DISABLED", "channel disabled")
			return
		}

		// Decode payload. We allow any JSON object — fields are passed
		// verbatim to the template engine. Reject non-object roots.
		// MaxBytesReader hard-stops the decoder at 1 MiB and surfaces
		// http.MaxBytesError so we can return 413 instead of OOM-ing.
		var payload map[string]any
		r.Body = http.MaxBytesReader(w, r.Body, MaxPushBodyBytes)
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		if err := dec.Decode(&payload); err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, r, http.StatusRequestEntityTooLarge, "VALIDATION",
					"push body exceeds 1 MiB limit")
				return
			}
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "body must be a JSON object: "+err.Error())
			return
		}
		// Trailing garbage is rejected to match the strict-input contract.
		if dec.More() {
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "unexpected trailing content")
			return
		}

		// dedup_key is an OPTIONAL top-level string; non-string values
		// or absence both fall back to canonical-fingerprint dedup
		// (handled inside pipeline.Ingest).
		var dedupKey string
		if v, ok := payload["dedup_key"]; ok {
			if s, ok := v.(string); ok {
				dedupKey = s
			}
		}

		pushID, dropped, err := deps.Ingest.Ingest(r.Context(), ch, payload, dedupKey)
		if err != nil {
			writeInternal(w, r, deps, "push: ingest", err)
			return
		}
		if dropped {
			writeJSON(w, http.StatusOK, map[string]any{
				"dropped": true,
				"reason":  "dedup",
			})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{
			"push_id": pushID,
			"status":  "queued",
		})
	}
}
