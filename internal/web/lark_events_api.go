// Package web — lark_events_api implements the inbound webhook
// endpoint Lark hits when a user @mentions the application bot or
// sends it a direct message. The endpoint is mounted at
//
//	POST /api/v1/lark/events
//
// PUBLICLY (no session middleware) because Lark cannot present a
// PulseGuard session cookie — every call must be authenticated by
// header signature verification against the bot row's encrypt_key.
// The route still inherits the global IP rate limiter so a misbehaving
// publisher cannot starve the rest of the API.
//
// Two distinct flows traverse this handler:
//
//  1. URL verification — Lark's first call after the operator pastes
//     the webhook URL in the developer console. Body is
//     {"challenge":"<random>","type":"url_verification"} and we
//     must echo {"challenge":"<same>"} verbatim. NO signature is
//     present on this call per Lark's documented protocol.
//
//  2. Event callback — every subsequent inbound message. Headers
//     X-Lark-Signature, X-Lark-Request-Timestamp, X-Lark-Request-Nonce
//     are required; we compute HMAC-SHA256(encrypt_key, timestamp +
//     nonce + body) and compare in constant time. Bot identity is
//     resolved from the body's event.app_id field via a tenant-blind
//     ListAll scan keyed by AppID — the multi-tenant model means
//     we cannot use URL path parameters or hostnames to identify
//     the bot.
//
// LB5 ships only the URL-verification + signature path. LB6 wires
// the actual dispatch to cmdrun.Dispatcher.
package web

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/wendi/pulseguard/internal/domain"
)

// MaxLarkEventBodyBytes caps the inbound payload. Lark event bodies
// are small (≤ a few KiB for text events, hundreds for cards); 64 KiB
// is generous without exposing a DoS vector through SQLite or memory
// pressure. Match the docs guidance "events are limited to 64 KB" so
// anyone shipping a larger custom payload is told to compress first.
const MaxLarkEventBodyBytes = 64 << 10

// LarkSignatureHeader / LarkTimestampHeader / LarkNonceHeader are the
// header names Lark sends with every event callback. They are public
// constants so tests can pin the wire contract.
const (
	LarkSignatureHeader = "X-Lark-Signature"
	LarkTimestampHeader = "X-Lark-Request-Timestamp"
	LarkNonceHeader     = "X-Lark-Request-Nonce"
)

// installLarkEventsRoutes mounts POST /api/v1/lark/events under the
// public sub-router (alongside /push). NO auth / CSRF middleware is
// applied — the body signature is the authentication mechanism, and
// CSRF makes no sense for a server-to-server callback.
//
// The route is mounted as a sibling of the push API so the same
// IP rate limiter (installed at /api/v1) protects it.
func installLarkEventsRoutes(r chi.Router, deps Deps) {
	r.Post("/lark/events", apiLarkEvents(deps))
}

// larkURLVerificationReq is the body shape for Lark's URL-verification
// handshake. type is always "url_verification" on this flow; we match
// on type rather than on the presence of "challenge" so a malicious
// publisher cannot trigger the no-signature branch by smuggling a
// "challenge" field into a real event.
type larkURLVerificationReq struct {
	Challenge string `json:"challenge"`
	Token     string `json:"token"`
	Type      string `json:"type"`
}

// larkEventEnvelope is the top-level body Lark posts on event callbacks.
// We only need event.app_id to resolve the bot row (and, transitively,
// the encrypt_key for signature verification + the tenant id for
// downstream dispatch). All other fields stay as raw JSON until LB6.
//
// The `schema` field is "2.0" on the v2 event protocol; we tolerate
// older "1.0" payloads by inspecting both `header.app_id` (v2) and
// `event.app_id` (v1) when resolving the bot row.
type larkEventEnvelope struct {
	Schema string `json:"schema"`
	Type   string `json:"type"`
	Header struct {
		EventID    string `json:"event_id"`
		EventType  string `json:"event_type"`
		AppID      string `json:"app_id"`
		TenantKey  string `json:"tenant_key"`
		CreateTime string `json:"create_time"`
		Token      string `json:"token"`
	} `json:"header"`
	Event json.RawMessage `json:"event"`
}

// larkEventV1Probe is the secondary shape Lark v1 events expose: the
// app_id lives inside the event payload itself rather than in a top-
// level "header" object. We probe both paths so the operator never
// has to think about schema versions.
type larkEventV1Probe struct {
	Type  string `json:"type"`
	Event struct {
		AppID string `json:"app_id"`
	} `json:"event"`
}

// apiLarkEvents handles the Lark inbound webhook. The handler is
// structured as a pure function over the request body + bot repo so
// every branch is reachable from tests without a real network.
func apiLarkEvents(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Cap the body up front. MaxBytesReader returns http.MaxBytesError
		// after the limit is crossed so we can distinguish DoS from
		// regular parse failures.
		r.Body = http.MaxBytesReader(w, r.Body, MaxLarkEventBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeError(w, r, http.StatusRequestEntityTooLarge, "VALIDATION",
					"lark event body exceeds 64 KiB limit")
				return
			}
			writeError(w, r, http.StatusBadRequest, "VALIDATION", "read body: "+err.Error())
			return
		}

		// URL-verification path: NO signature, simply echo the challenge.
		// We detect it BEFORE signature verification because per Lark
		// docs the verify-URL handshake intentionally omits the
		// X-Lark-Signature header (the operator has not yet pasted the
		// encrypt_key into the console at that point).
		if verifyReq, ok := tryParseURLVerification(body); ok {
			writeJSON(w, http.StatusOK, map[string]string{"challenge": verifyReq.Challenge})
			return
		}

		// Resolve the bot row before signature verification — we need
		// its encrypt_key to compute the expected HMAC. The lookup is
		// scoped by app_id; Lark guarantees one app per (event_id, app_id)
		// triple so the tenant-blind ListAll scan is correct (and
		// O(bots), which at our scale is in the low hundreds).
		appID, ok := extractAppID(body)
		if !ok || appID == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION",
				"lark event missing app_id")
			return
		}
		bot, err := findLarkAppBot(r, deps, appID)
		if err != nil {
			if errors.Is(err, domain.ErrNotFound) {
				// Unknown bot — return 404 (operator pasted the URL
				// into a Lark app whose credentials are not in PulseGuard).
				writeError(w, r, http.StatusNotFound, "NOT_FOUND",
					"no PulseGuard bot configured for this app_id")
				return
			}
			writeInternal(w, r, deps, "lark events: bot lookup", err)
			return
		}
		if !bot.Enabled {
			writeError(w, r, http.StatusGone, "BOT_DISABLED", "bot disabled")
			return
		}
		if bot.EncryptKey == "" {
			// A bot row that exists but has no encrypt_key cannot
			// authenticate any inbound event. Reject with 412 so the
			// operator sees a precise error in Lark's developer console.
			writeError(w, r, http.StatusPreconditionFailed, "VALIDATION",
				"bot has no encrypt_key configured")
			return
		}

		// Signature verification.
		sigHeader := r.Header.Get(LarkSignatureHeader)
		tsHeader := r.Header.Get(LarkTimestampHeader)
		nonceHeader := r.Header.Get(LarkNonceHeader)
		if sigHeader == "" || tsHeader == "" || nonceHeader == "" {
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
				"missing lark signature headers")
			return
		}
		if !verifyLarkSignature(bot.EncryptKey, tsHeader, nonceHeader, body, sigHeader) {
			writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
				"lark signature mismatch")
			return
		}

		// LB5 stops here — LB6 will hand the verified envelope to the
		// dispatcher. For now ACK with 200 so Lark's retry queue
		// doesn't flood while events land for a bot that has not yet
		// been migrated to the dispatcher.
		writeJSON(w, http.StatusOK, map[string]any{"status": "accepted"})
	}
}

// tryParseURLVerification probes whether body is the Lark URL-
// verification handshake. Returns (parsed, true) on a positive match,
// (zero, false) otherwise. The check is type=="url_verification" AND
// a non-empty challenge so a misconfigured publisher cannot trigger
// the no-signature shortcut.
func tryParseURLVerification(body []byte) (larkURLVerificationReq, bool) {
	var req larkURLVerificationReq
	if err := json.Unmarshal(body, &req); err != nil {
		return larkURLVerificationReq{}, false
	}
	if req.Type != "url_verification" || req.Challenge == "" {
		return larkURLVerificationReq{}, false
	}
	return req, true
}

// extractAppID hunts for the app_id across both v1 and v2 event
// shapes. Returns (id, true) on success; an empty string with true
// counts as "field present but empty" so the caller can return a
// specific 400 instead of treating it as a parse failure.
func extractAppID(body []byte) (string, bool) {
	// v2 shape: header.app_id.
	var env larkEventEnvelope
	if err := json.Unmarshal(body, &env); err == nil && env.Header.AppID != "" {
		return env.Header.AppID, true
	}
	// v1 shape: event.app_id.
	var v1 larkEventV1Probe
	if err := json.Unmarshal(body, &v1); err == nil && v1.Event.AppID != "" {
		return v1.Event.AppID, true
	}
	return "", true
}

// findLarkAppBot scans every bot for the (Platform=lark, BotKind=app,
// AppID=appID) row. The scan is tenant-blind by design — the body
// has not been authenticated yet, so we cannot trust any tenant
// hint inside it. Returns ErrNotFound when no matching row exists.
func findLarkAppBot(r *http.Request, deps Deps, appID string) (*domain.Bot, error) {
	bots, err := deps.Bots.ListAll(r.Context())
	if err != nil {
		return nil, err
	}
	for _, b := range bots {
		if b.Platform == domain.PlatformLark && b.BotKind == domain.BotKindApp && b.AppID == appID {
			return b, nil
		}
	}
	return nil, domain.ErrNotFound
}

// verifyLarkSignature recomputes the HMAC-SHA256(encryptKey,
// timestamp+nonce+body) hex digest and compares constant-time
// against the X-Lark-Signature header. The order — timestamp then
// nonce then body, with no separators — matches Lark's documented
// algorithm exactly; any tampering with either header or the body
// flips the digest.
//
// Returns true when the header matches the recomputed value. A
// missing or malformed header counts as false (the caller already
// rejected empty strings before reaching here).
func verifyLarkSignature(encryptKey, timestamp, nonce string, body []byte, gotHex string) bool {
	mac := hmac.New(sha256.New, []byte(encryptKey))
	mac.Write([]byte(timestamp))
	mac.Write([]byte(nonce))
	mac.Write(body)
	wantHex := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(wantHex), []byte(gotHex))
}

// ComputeLarkSignature exposes verifyLarkSignature's helper for tests
// (and for any future mock publisher). Returning the hex string lets
// the same code path produce both the "expected" and "transmitted"
// values without reimplementation.
func ComputeLarkSignature(encryptKey, timestamp, nonce string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(encryptKey))
	mac.Write([]byte(timestamp))
	mac.Write([]byte(nonce))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// reuseBody returns a request body reader that re-reads the bytes we
// already drained. Currently unused outside tests but kept so LB6
// can call it when forwarding the envelope to the dispatcher
// without an extra copy.
func reuseBody(body []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(body))
}
