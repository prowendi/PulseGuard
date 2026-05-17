package web

import (
	"errors"
	"net/http"
	"regexp"
	"strconv"

	"github.com/wendi/pulseguard/internal/domain"

	"github.com/go-chi/chi/v5"
)

// MaxCommandCodeBytes caps the Starlark source size accepted by the
// commands API/UI. The Starlark compiler is single-threaded per script
// and runs inside the request lifetime, so unbounded source lets a
// tenant pin a worker / inflate SQLite. 64 KiB covers any sane custom
// command (the bundled demos in command_ui.go are <200 bytes each) and
// still leaves headroom for embedded constants. The MaxBytesReader on
// the request body remains the outer guard (1 MiB) so this only kicks
// in when the JSON body is dominated by the code field.
//
// Refs: round2-security-report S2-M1.
const MaxCommandCodeBytes = 64 * 1024

// botTokenPattern is the canonical Telegram bot token format:
// `<bot_id>:<token>` where the second part is base64url-ish.
var botTokenPattern = regexp.MustCompile(`^\d+:[A-Za-z0-9_-]+$`)

// larkWebhookPattern mirrors lark.webhookPattern so the web validation
// layer can reject a Lark-shaped token that points at the wrong host
// (or that a copy-paste accidentally truncated) before the row hits
// the DB. Keeping it duplicated here is the lesser evil — importing
// the lark package from web would entangle the dependency graph for
// one constant.
var larkWebhookPattern = regexp.MustCompile(`^https://open\.feishu\.cn/open-apis/bot/v2/hook/[A-Za-z0-9_\-]+/?$`)

// botTokenLooksValid checks the token shape for the given platform.
// Each platform stores a different credential in domain.Bot.BotToken:
//
//   - PlatformTelegram → "<bot_id>:<token>"
//   - PlatformLark     → "https://open.feishu.cn/open-apis/bot/v2/hook/<key>"
//
// The function is shape-only; it does not call out to the upstream
// API (that's the worker's job at delivery time). Returns false for
// an unknown platform so the caller can surface a clean VALIDATION
// error instead of accepting a credential that no Sender knows how
// to consume.
func botTokenLooksValid(platform, token string) bool {
	switch platform {
	case domain.PlatformTelegram:
		return botTokenPattern.MatchString(token)
	case domain.PlatformLark:
		return larkWebhookPattern.MatchString(token)
	default:
		return false
	}
}

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
// Use it from CRUD handlers so they don't repeat the switch. The
// default branch (unknown errors) is sanitised via writeInternal so we
// never leak a raw err.Error() into a 5xx body.
func writeRepoError(w http.ResponseWriter, r *http.Request, deps Deps, err error) {
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
		writeInternal(w, r, deps, "repo error", err)
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

// validateCommandCode enforces the MaxCommandCodeBytes ceiling on the
// raw Starlark source. The caller has already trimmed empty input via
// strings.TrimSpace; this only guards the upper bound so we never
// hand a multi-MiB blob to the Starlark compiler. Returns ok=false
// after writing a VALIDATION 400 — the handler should `return`.
func validateCommandCode(w http.ResponseWriter, r *http.Request, code string) bool {
	if len(code) > MaxCommandCodeBytes {
		writeError(w, r, http.StatusBadRequest, "VALIDATION",
			"code too large (max "+strconv.Itoa(MaxCommandCodeBytes)+" bytes)")
		return false
	}
	return true
}
