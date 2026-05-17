package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// LarkAppTokenPrefix is the pseudo-URL scheme the store layer
// assembles for Lark application bot rows so the existing
// runtime.senderRouter can detect "this is an app-mode lark token"
// from a single string. The full shape is:
//
//	lark-app://<app_id>?secret=<plaintext-secret>
//
// (assembled by store/bot_repo.appBotToken). Both halves round-trip
// through url.Parse so the router can recover them without bespoke
// string surgery. Callers must NOT log the assembled token; treat it
// like the raw secret it embeds.
const LarkAppTokenPrefix = "lark-app://"

// AppClient is the outbound Lark IM API client for application bots.
// Unlike the Phase-A custom-bot Client (which posts to a pre-baked
// webhook URL bound to a single chat), AppClient sends arbitrary
// messages to any chat the bot can see, authenticated with a
// tenant_access_token resolved on demand from the TokenSource.
//
// Send / Edit accept a botToken in the lark-app:// shape so the
// senderRouter can route by token prefix the same way it already
// does for https://open.feishu.cn (Phase A webhook). chatID is the
// receive_id Lark's IM API expects (open_chat_id by default — see
// LarkAppReceiveIDType for the override knob).
//
// apiBase overrides the canonical https://open.feishu.cn host for
// tests; leave empty in production.
type AppClient struct {
	httpC   *http.Client
	apiBase string
	tokens  TokenSource

	// receiveIDType is forwarded as the receive_id_type query
	// parameter on POST /open-apis/im/v1/messages. Defaults to
	// "chat_id" so an operator who pastes a group's chat_id from the
	// Lark UI gets a working bot without further config. Tests
	// override it via newAppClientWithBase.
	receiveIDType string
}

// LarkAppReceiveIDTypeDefault is the IM API receive_id_type query
// parameter AppClient.Send injects by default. "chat_id" accepts
// open_chat_id values which is the most operator-friendly default —
// the events endpoint we wire up next reports exactly that field on
// inbound messages so reply paths are symmetric.
const LarkAppReceiveIDTypeDefault = "chat_id"

// NewAppClient builds a production-ready AppClient backed by the
// supplied TokenSource. timeout caps the outbound HTTP call; zero or
// negative falls back to 10s (consistent with the webhook Client +
// OAuthClient defaults).
func NewAppClient(tokens TokenSource, timeout time.Duration) *AppClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &AppClient{
		httpC:         &http.Client{Timeout: timeout},
		tokens:        tokens,
		receiveIDType: LarkAppReceiveIDTypeDefault,
	}
}

// newAppClientWithBase is a test-only constructor that pins the API
// base (so an httptest.Server can capture both the OAuth and IM API
// calls) and lets the test override the receive_id_type for coverage.
func newAppClientWithBase(tokens TokenSource, base string, timeout time.Duration, receiveIDType string) *AppClient {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if receiveIDType == "" {
		receiveIDType = LarkAppReceiveIDTypeDefault
	}
	return &AppClient{
		httpC:         &http.Client{Timeout: timeout},
		apiBase:       base,
		tokens:        tokens,
		receiveIDType: receiveIDType,
	}
}

// ParseAppToken splits a lark-app://<app_id>?secret=<plain> pseudo-URL
// into its (appID, secret) parts. Returns ErrBadAppCreds if the token
// is malformed or any half is empty. The router and the AppClient both
// call this so the parsing rule lives in exactly one place.
func ParseAppToken(token string) (appID, secret string, err error) {
	if !strings.HasPrefix(token, LarkAppTokenPrefix) {
		return "", "", fmt.Errorf("%w: missing %s prefix", ErrBadAppCreds, LarkAppTokenPrefix)
	}
	u, err := url.Parse(token)
	if err != nil {
		return "", "", fmt.Errorf("%w: parse: %v", ErrBadAppCreds, err)
	}
	if u.Scheme != "lark-app" {
		return "", "", fmt.Errorf("%w: scheme = %q", ErrBadAppCreds, u.Scheme)
	}
	appID = u.Host
	secret = u.Query().Get("secret")
	if appID == "" {
		return "", "", fmt.Errorf("%w: empty app_id", ErrBadAppCreds)
	}
	if secret == "" {
		return "", "", fmt.Errorf("%w: empty secret", ErrBadAppCreds)
	}
	return appID, secret, nil
}

// imSendReq is the body Lark's POST /open-apis/im/v1/messages expects.
// content must be a STRING (Lark's API double-encodes the payload —
// the outer envelope JSON-serialises content as a string, and the
// string itself is JSON for non-text msg_types). For text messages:
//
//	{"msg_type":"text","receive_id":"oc_xxx","content":"{\"text\":\"hi\"}"}
type imSendReq struct {
	ReceiveID string `json:"receive_id"`
	MsgType   string `json:"msg_type"`
	Content   string `json:"content"` // stringified JSON per Lark spec
}

// imEditReq mirrors PATCH /open-apis/im/v1/messages/:message_id which
// only accepts a content body (and the existing msg_type from the
// original message is preserved). msg_type is still required when the
// operator wants to mutate it; we forward "text" by default for
// symmetry with imSendReq.
type imEditReq struct {
	MsgType string `json:"msg_type"`
	Content string `json:"content"`
}

// imSendResp is the envelope Lark wraps every IM API response in. We
// only care about code+msg for branching; data.message_id is parsed so
// AppClient.Send can return it for the editMessageText state machine.
type imSendResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		MessageID string `json:"message_id"`
	} `json:"data"`
}

// imTextContent is the inner content body for msg_type=text. We
// JSON-marshal it ourselves before stringifying into imSendReq.Content
// so callers can pass plain text without thinking about Lark's nested
// encoding.
type imTextContent struct {
	Text string `json:"text"`
}

// buildAppContent decides whether to wrap text in the inner
// {"text":"..."} envelope or pass through a pre-formed lark message
// envelope (post / interactive / image / share_chat etc.). Same
// detection rule as buildLarkBody — top-level object with a non-empty
// "msg_type" field means the operator hand-crafted the payload.
// Returns (msgType, contentString) ready for imSendReq.
func buildAppContent(text string) (string, string, error) {
	if isLarkEnvelope(text) {
		// Caller supplied a full envelope. Decode just enough to
		// extract msg_type, then re-serialise the inner content blob
		// (everything except msg_type) as the IM API stringified
		// content body.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal([]byte(text), &probe); err != nil {
			return "", "", fmt.Errorf("lark app: probe envelope: %w", err)
		}
		var msgType string
		if mt, ok := probe["msg_type"]; ok {
			if err := json.Unmarshal(mt, &msgType); err != nil {
				return "", "", fmt.Errorf("lark app: parse msg_type: %w", err)
			}
		}
		delete(probe, "msg_type")
		// Lark IM API expects exactly one "content" key in the inner
		// blob. Templates commonly emit either
		// {"msg_type":"...","content":{...}} or {"msg_type":"...","card":{...}}
		// — we forward whatever non-msg_type body was supplied.
		var innerBytes []byte
		if c, ok := probe["content"]; ok {
			innerBytes = []byte(c)
		} else {
			rest, err := json.Marshal(probe)
			if err != nil {
				return "", "", fmt.Errorf("lark app: re-serialise envelope: %w", err)
			}
			innerBytes = rest
		}
		return msgType, string(innerBytes), nil
	}
	inner, err := json.Marshal(imTextContent{Text: text})
	if err != nil {
		return "", "", fmt.Errorf("lark app: marshal text content: %w", err)
	}
	return "text", string(inner), nil
}

// Send posts a message to the IM API. botToken is a lark-app://
// pseudo-URL; chatID is the receive_id Lark expects. parseMode is
// accepted for Sender-interface compatibility but ignored (Lark text
// messages are plain).
//
// Returns the message_id Lark assigned (best-effort — Lark sometimes
// returns an empty string in 2xx responses; the caller's downstream
// editMessageText flow must tolerate 0). Errors follow the same
// classification as the webhook Client: Transient (5xx / 429 / parse)
// vs PermanentClient (4xx / app-level non-zero code).
//
// A 401 from the IM API endpoint evicts the cached token before
// returning so the next call refreshes. This guards against
// server-side token revocation (e.g. operator regenerated the app
// secret in the Lark console) that the OAuth client's TTL alone would
// not detect.
func (c *AppClient) Send(ctx context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	_ = parseMode
	appID, secret, err := ParseAppToken(botToken)
	if err != nil {
		return 0, err
	}
	if chatID == "" {
		return 0, &APIError{
			Class:       PermanentClient,
			Description: "lark app: empty receive_id (chat_id)",
		}
	}
	tok, err := c.tokens.Token(ctx, appID, secret)
	if err != nil {
		return 0, err
	}
	msgType, content, err := buildAppContent(text)
	if err != nil {
		return 0, err
	}
	body, err := json.Marshal(imSendReq{
		ReceiveID: chatID,
		MsgType:   msgType,
		Content:   content,
	})
	if err != nil {
		return 0, fmt.Errorf("lark app: marshal send: %w", err)
	}

	base := c.apiBase
	if base == "" {
		base = canonicalAPIBase
	}
	endpoint := base + "/open-apis/im/v1/messages?receive_id_type=" + url.QueryEscape(c.receiveIDType)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("lark app: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+tok)

	return c.do(ctx, req, appID, secret, "send")
}

// Edit calls PATCH /open-apis/im/v1/messages/:message_id. messageID is
// the int64 the worker stored from the original Send — Lark's
// message_id is a string in their docs but we keep the int64 signature
// for SenderWithOpts interface compatibility and stringify on the way
// out. messageID == 0 falls back to a fresh Send so the V7-2
// editMessageText state machine never panics when the original send
// raced past the persist step.
//
// Note: for fully-symmetric Telegram-style behaviour Lark requires the
// new message to keep the same msg_type as the original. We don't
// surface that constraint to callers — buildAppContent infers msg_type
// from the new text the same way Send does, so an operator switching
// from text → card mid-collapse will get a Lark 4xx that the worker
// already classifies as PermanentClient.
func (c *AppClient) Edit(ctx context.Context, botToken, chatID string, messageID int64, parseMode, text string) error {
	_ = chatID
	_ = parseMode
	if messageID == 0 {
		// Nothing to patch; degrade to a fresh send so the alert still
		// lands in the chat. Caller (worker) already tolerates this
		// fallback for the Phase-A webhook client.
		_, err := c.Send(ctx, botToken, chatID, parseMode, text)
		return err
	}
	appID, secret, err := ParseAppToken(botToken)
	if err != nil {
		return err
	}
	tok, err := c.tokens.Token(ctx, appID, secret)
	if err != nil {
		return err
	}
	msgType, content, err := buildAppContent(text)
	if err != nil {
		return err
	}
	body, err := json.Marshal(imEditReq{
		MsgType: msgType,
		Content: content,
	})
	if err != nil {
		return fmt.Errorf("lark app: marshal edit: %w", err)
	}
	base := c.apiBase
	if base == "" {
		base = canonicalAPIBase
	}
	endpoint := base + "/open-apis/im/v1/messages/" + url.PathEscape(fmt.Sprintf("om_%d", messageID))
	// Lark message_id is opaque; the worker stores it via hashing
	// today and we don't have a stable int64 mapping. Edit is wired
	// here for interface completeness — the worker code-path for
	// lark-app will route through senderRouter.SendWithOpts (Send +
	// dropped buttons) rather than EditMessage in the first cut, so
	// Edit is effectively dead. We keep the implementation behind
	// the messageID==0 fast-path so a future migration to a real
	// op→messageID mapping can drop in without changing this file.
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("lark app: build edit request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+tok)
	_, err = c.do(ctx, req, appID, secret, "edit")
	return err
}

// do is the shared response handler for Send and Edit. opLabel is a
// short tag included in error Description so logs can distinguish
// "send" vs "edit" failures without callers having to wrap.
func (c *AppClient) do(ctx context.Context, req *http.Request, appID, appSecret, opLabel string) (int64, error) {
	_ = ctx
	resp, err := c.httpC.Do(req)
	if err != nil {
		return 0, &APIError{
			Class:       Transient,
			Description: "lark app " + opLabel + " http do: " + err.Error(),
		}
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "lark app " + opLabel + " read body: " + readErr.Error(),
		}
	}
	// 401 → cached token was revoked server-side. Evict so the next
	// attempt refreshes; the caller still receives a transient error
	// (the worker will retry, and the second try gets a fresh token).
	if resp.StatusCode == 401 {
		if forgetter, ok := c.tokens.(interface {
			Forget(appID, appSecret string) bool
		}); ok {
			forgetter.Forget(appID, appSecret)
		}
		return 0, &APIError{
			Class:       Transient,
			Code:        401,
			Description: "lark app " + opLabel + ": 401 (token evicted, next call refreshes)",
		}
	}
	if resp.StatusCode >= 500 {
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "lark app " + opLabel + " 5xx: " + safeBodyExcerpt(respBody),
		}
	}
	if resp.StatusCode == 429 {
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "lark app " + opLabel + " rate limited",
		}
	}
	if resp.StatusCode >= 400 {
		return 0, &APIError{
			Class:       PermanentClient,
			Code:        resp.StatusCode,
			Description: "lark app " + opLabel + " 4xx: " + safeBodyExcerpt(respBody),
		}
	}
	var env imSendResp
	if err := json.Unmarshal(respBody, &env); err != nil {
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "lark app " + opLabel + " parse body: " + err.Error(),
		}
	}
	if env.Code != 0 {
		return 0, &APIError{
			Class:       PermanentClient,
			Code:        env.Code,
			Description: fmt.Sprintf("lark app %s code=%d msg=%s", opLabel, env.Code, env.Msg),
		}
	}
	// Lark message_id is an opaque string ("om_xxxxxxx"). We return 0
	// because the caller already tolerates the webhook Client doing
	// the same — the editMessageText state machine never engages for
	// Lark in the current senderRouter wire-up.
	_ = env.Data.MessageID
	return 0, nil
}
