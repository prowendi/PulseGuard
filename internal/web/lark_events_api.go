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
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prowendi/PulseGuard/internal/domain"
	"github.com/prowendi/PulseGuard/internal/lark"
	"github.com/prowendi/PulseGuard/internal/scripting"
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
		//
		// SEC-4 (2026-05): every authentication-stage failure path
		// — unknown bot, disabled bot, bot missing encrypt_key,
		// missing headers, signature mismatch, stale timestamp — must
		// return the SAME response shape (401 UNAUTHENTICATED with
		// generic body). Distinguishing 404 / 410 / 412 lets an
		// attacker enumerate which Lark app_ids are registered on this
		// PulseGuard instance and which of them have signing keys.
		// The internal reason is logged server-side but never echoed.
		appID, ok := extractAppID(body)
		if !ok || appID == "" {
			writeError(w, r, http.StatusBadRequest, "VALIDATION",
				"lark event missing app_id")
			return
		}
		bot, lookupErr := findLarkAppBot(r, deps, appID)
		if lookupErr != nil && !errors.Is(lookupErr, domain.ErrNotFound) {
			writeInternal(w, r, deps, "lark events: bot lookup", lookupErr)
			return
		}
		// Past this point we collapse every reachable auth failure
		// into a single response. rejectAuth logs the reason but emits
		// a uniform 401 to the wire.
		if bot == nil || !bot.Enabled || bot.EncryptKey == "" {
			rejectLarkAuth(w, r, deps, "bot resolve/enabled/key check failed",
				"app_id", appID,
				"bot_nil", bot == nil,
				"enabled", bot != nil && bot.Enabled,
				"encrypt_key_set", bot != nil && bot.EncryptKey != "")
			return
		}

		// Signature verification.
		sigHeader := r.Header.Get(LarkSignatureHeader)
		tsHeader := r.Header.Get(LarkTimestampHeader)
		nonceHeader := r.Header.Get(LarkNonceHeader)
		if sigHeader == "" || tsHeader == "" || nonceHeader == "" {
			rejectLarkAuth(w, r, deps, "missing signature headers",
				"bot_id", bot.ID)
			return
		}
		// SEC-5: reject events whose timestamp is more than 5 minutes
		// off from server clock. Without this an attacker who once
		// captured a valid signed request could replay it forever; with
		// it, a replay window only exists for the time the signature
		// is fresh. Lark's docs recommend the same 5-minute bound.
		if !isLarkTimestampFresh(tsHeader, deps) {
			rejectLarkAuth(w, r, deps, "stale or invalid timestamp",
				"bot_id", bot.ID, "timestamp", tsHeader)
			return
		}
		if !verifyLarkSignature(bot.EncryptKey, tsHeader, nonceHeader, body, sigHeader) {
			rejectLarkAuth(w, r, deps, "signature mismatch",
				"bot_id", bot.ID)
			return
		}

		// Past this point the inbound is authenticated. LB6: parse the
		// IM message event, extract /command name+args, dispatch to
		// Starlark, then reply via the AppClient by reusing the
		// runtime senderRouter (deps.TG) — the lark-app:// derived
		// BotToken on the bot row routes the reply to the right
		// AppClient automatically.
		msg, ok := parseInboundMessage(body)
		if !ok {
			// Not a message event we know how to handle — ack with 200
			// so Lark stops retrying. This covers card-action events,
			// member-join events, etc., which LB6 deliberately
			// ignores.
			writeJSON(w, http.StatusOK, map[string]any{"status": "accepted"})
			return
		}
		name, args, ok := parseSlashCommand(msg.text)
		if !ok {
			// Non-slash messages (plain chat, mentions without a
			// command, etc.) are acknowledged silently.
			writeJSON(w, http.StatusOK, map[string]any{"status": "accepted"})
			return
		}
		reply, dispatched := dispatchLarkCommand(r.Context(), deps, bot, msg.chatID, name, args)
		if !dispatched {
			// Unknown / disabled command: stay silent (matches the
			// telegram listener's ErrDispatchSkip semantics).
			writeJSON(w, http.StatusOK, map[string]any{"status": "accepted"})
			return
		}
		// Send the reply. We bypass deps.TG (the production
		// senderRouter) when it is nil — tests can substitute a fake
		// sender via deps.TG; if neither is wired we just log the
		// payload at info.
		if deps.TG != nil && reply != "" {
			if _, sendErr := deps.TG.Send(r.Context(), bot.BotToken, msg.chatID, "", reply); sendErr != nil {
				if deps.Logger != nil {
					deps.Logger.Warn("lark events: reply send failed",
						"bot_id", bot.ID,
						"tenant_id", bot.TenantID,
						"err", sendErr.Error())
				}
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "dispatched"})
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

// inboundMessage is the projection of an im.message.receive_v1 event
// the LB6 dispatcher cares about: the plain-text body the user typed
// plus the chat_id we reply to. open_chat_id is preferred over chat_id
// because Lark's IM API expects the open_chat_id when receive_id_type
// is "chat_id" (the AppClient default).
type inboundMessage struct {
	chatID string
	text   string
}

// parseInboundMessage extracts (chatID, text) from a v2 event body.
// Returns (zero, false) for any non-text message_receive event so
// the handler ACKs silently rather than producing noise.
//
// The Lark IM event shape is doubly-encoded: event.message.content
// is a STRING containing JSON like {"text":"/echo hi"}. We unmarshal
// twice to recover the raw user text.
func parseInboundMessage(body []byte) (inboundMessage, bool) {
	var env struct {
		Header struct {
			EventType string `json:"event_type"`
		} `json:"header"`
		Event struct {
			Message struct {
				ChatID      string `json:"chat_id"`
				OpenChatID  string `json:"open_chat_id"`
				MessageType string `json:"message_type"`
				Content     string `json:"content"`
			} `json:"message"`
		} `json:"event"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return inboundMessage{}, false
	}
	if env.Header.EventType != "" && env.Header.EventType != "im.message.receive_v1" {
		return inboundMessage{}, false
	}
	if env.Event.Message.MessageType != "text" {
		return inboundMessage{}, false
	}
	// Doubly-encoded content: parse the inner JSON object to extract
	// the user's text. {"text":"<actual user text>"}.
	var inner struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(env.Event.Message.Content), &inner); err != nil {
		return inboundMessage{}, false
	}
	chat := env.Event.Message.OpenChatID
	if chat == "" {
		chat = env.Event.Message.ChatID
	}
	if chat == "" || inner.Text == "" {
		return inboundMessage{}, false
	}
	return inboundMessage{chatID: chat, text: inner.Text}, true
}

// parseSlashCommand splits a "/name arg1 arg2 ..." string into (name,
// args, true) when the input starts with "/", or (zero, false)
// otherwise. The name is stripped of the leading "/" AND of any
// "@botname" suffix the Lark client appends when users tap the
// inline command picker, matching the Telegram listener's
// normalisation rules.
func parseSlashCommand(text string) (string, []string, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return "", nil, false
	}
	trimmed = strings.TrimPrefix(trimmed, "/")
	if trimmed == "" {
		return "", nil, false
	}
	parts := strings.Fields(trimmed)
	if len(parts) == 0 {
		return "", nil, false
	}
	name := parts[0]
	if i := strings.IndexByte(name, '@'); i >= 0 {
		// "@bot" with no preceding command is invalid (i==0); a non-
		// zero index means the user typed "/cmd@bot ..." and we
		// keep only the bit before the '@'.
		name = name[:i]
	}
	if name == "" {
		return "", nil, false
	}
	return name, parts[1:], true
}

// dispatchLarkCommand resolves the command via deps.Commands, executes
// it through deps.ScriptExec, upserts the Lark subscriber, and
// returns (replyText, dispatched). When the command is unknown /
// disabled or no executor is wired, dispatched=false and the caller
// stays silent.
//
// We deliberately do not reuse the Telegram-typed cmdrun.Dispatcher
// here because its DispatchInput uses int64 ChatID, which doesn't
// round-trip Lark's opaque "oc_..." identifiers. The duplication is
// small (~30 lines) and avoids introducing a wider interface change
// for a single new platform.
func dispatchLarkCommand(ctx context.Context, deps Deps, bot *domain.Bot, chatID, name string, args []string) (string, bool) {
	if deps.Commands == nil || deps.ScriptExec == nil {
		return "", false
	}
	cmd, err := resolveLarkCommand(ctx, deps, bot.ID, name)
	if err != nil {
		// Unknown / disabled / repo error — stay silent. Repo errors
		// are logged so on-call still sees them; the user just gets
		// no reply.
		if !errors.Is(err, domain.ErrNotFound) && deps.Logger != nil {
			deps.Logger.Warn("lark events: command resolve failed",
				"bot_id", bot.ID,
				"tenant_id", bot.TenantID,
				"name", name,
				"err", err.Error())
		}
		return "", false
	}
	// Upsert subscriber before executing so a slow / failed script
	// still leaves an audit trail of who tried what.
	if deps.Subscribers != nil {
		upErr := deps.Subscribers.Upsert(ctx, &domain.Subscriber{
			TenantID:  cmd.TenantID,
			CommandID: cmd.ID,
			BotID:     bot.ID,
			ChatID:    chatID,
			Platform:  domain.PlatformLark,
		})
		if upErr != nil && deps.Logger != nil {
			deps.Logger.Warn("lark events: subscriber upsert failed",
				"bot_id", bot.ID,
				"tenant_id", bot.TenantID,
				"command_id", cmd.ID,
				"err", upErr.Error())
		}
	}
	res, runErr := deps.ScriptExec.Execute(ctx, cmd.Code, args)
	if runErr != nil {
		if deps.Logger != nil {
			deps.Logger.Warn("lark events: command execution failed",
				"bot_id", bot.ID,
				"tenant_id", bot.TenantID,
				"command_id", cmd.ID,
				"err", runErr.Error())
		}
		// Surface a generic Chinese-friendly fail message so the user
		// in the Lark chat sees the dispatch ran. Distinguishing
		// timeout / unsafe-host / etc. is not worth the extra
		// branches at this layer — the operator gets the precise
		// classification in the structured log.
		return fmt.Sprintf("命令 %q 执行失败", name), true
	}
	return stitchScriptResult(res), true
}

// resolveLarkCommand mirrors cmdrun.Dispatcher.resolve: try the "/"+
// prefixed form first (UI convention), then the bare name. Returns
// ErrNotFound when neither shape matches an enabled row.
func resolveLarkCommand(ctx context.Context, deps Deps, botID int64, name string) (*domain.Command, error) {
	candidates := []string{"/" + name, name}
	for _, n := range candidates {
		c, err := deps.Commands.GetByBotAndName(ctx, botID, n)
		if err == nil {
			if !c.Enabled {
				continue
			}
			return c, nil
		}
		if !errors.Is(err, domain.ErrNotFound) {
			return nil, err
		}
	}
	return nil, domain.ErrNotFound
}

// stitchScriptResult joins Output + Return with a newline. Mirrors
// the cmdrun.stitch helper byte-for-byte so the inbound chat sees
// the same envelope shape Telegram users get.
func stitchScriptResult(r *scripting.Result) string {
	if r == nil {
		return ""
	}
	var parts []string
	if s := strings.TrimSpace(r.Output); s != "" {
		parts = append(parts, s)
	}
	if s := strings.TrimSpace(r.Return); s != "" {
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

// unused but referenced for future expansion. Keeps the lark import
// alive without sprinkling underscore-imports elsewhere.
var _ = lark.LarkAppTokenPrefix

// LarkEventTimestampSkew bounds how far an X-Lark-Request-Timestamp
// header may diverge from server clock before we reject the event.
// 5 minutes matches the Lark documented recommendation and is large
// enough to tolerate operator clock skew without giving an attacker
// who once captured a valid signed request a forever-replay window.
const LarkEventTimestampSkew = 5 * time.Minute

// isLarkTimestampFresh validates the X-Lark-Request-Timestamp header
// against the server clock with the documented ±LarkEventTimestampSkew
// window. Returns false for missing, malformed, or out-of-window
// timestamps so the caller can reject with a uniform 401.
//
// deps is plumbed so future clock injection (for tests) can come
// through without rewriting callers; today we use time.Now().
func isLarkTimestampFresh(tsHeader string, _ Deps) bool {
	if tsHeader == "" {
		return false
	}
	tsInt, err := strconv.ParseInt(strings.TrimSpace(tsHeader), 10, 64)
	if err != nil || tsInt <= 0 {
		return false
	}
	delta := time.Since(time.Unix(tsInt, 0))
	if delta < 0 {
		delta = -delta
	}
	return delta <= LarkEventTimestampSkew
}

// rejectLarkAuth emits a uniform 401 UNAUTHENTICATED response and
// records the actual reason in server logs. SEC-4: every authentication
// failure on the events endpoint MUST produce the same wire response
// so an attacker cannot enumerate which app_ids exist, which are
// enabled, which have signing keys configured, or which signatures
// were close-but-wrong. The internal reason + structured fields stay
// in the operator log for diagnostics.
func rejectLarkAuth(w http.ResponseWriter, r *http.Request, deps Deps, reason string, kv ...any) {
	if deps.Logger != nil {
		args := []any{"endpoint", "/api/v1/lark/events", "reason", reason}
		args = append(args, kv...)
		deps.Logger.Info("lark events: authentication rejected", args...)
	}
	writeError(w, r, http.StatusUnauthorized, "UNAUTHENTICATED",
		"lark event authentication failed")
}
