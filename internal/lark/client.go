// Package lark implements the outbound client for Lark / 飞书 Custom Bot
// webhooks. Unlike Telegram, Lark custom bots are single-direction
// (push-only) — there is no long-poll API, no inline keyboard concept,
// and no editMessageText. The client therefore satisfies only the
// domain.Sender contract (and degrades EditMessage to a fresh Send for
// the SenderWithOpts adapter wired in runtime).
//
// The "bot token" stored on domain.Bot for a Lark bot is the full
// webhook URL handed out by the Lark group admin, e.g.
//
//	https://open.feishu.cn/open-apis/bot/v2/hook/<32-char-key>
//
// The chat is implicit in the URL — the Channel.ChatID column is
// unused for Lark and the worker passes whatever the operator typed
// straight through (validation at the web layer hints that it can be
// any non-empty string).
package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/wendi/pulseguard/internal/config"
)

// webhookPattern guards Send against accidentally hitting unrelated
// hosts when a misconfigured row carries a Telegram-shaped token. The
// canonical Lark custom-bot webhook is
//
//	https://open.feishu.cn/open-apis/bot/v2/hook/<key>
//
// We accept any non-empty path segment after /hook/ so future key
// rotations or longer formats keep working without a client change.
var webhookPattern = regexp.MustCompile(`^https://open\.feishu\.cn/open-apis/bot/v2/hook/[A-Za-z0-9_\-]+/?$`)

// Client is the outbound Lark webhook client. apiBase, when non-empty,
// overrides the canonical https://open.feishu.cn host so tests can
// redirect to an httptest.Server — when set, the BotToken's host is
// rewritten to apiBase before the POST. Production callers leave
// apiBase empty and pass the operator-supplied webhook URL through.
type Client struct {
	httpC   *http.Client
	apiBase string // optional override used only by tests
}

// New constructs a Client. The Telegram config struct is reused (no
// dedicated config.Lark yet — YAGNI per L2 spec) so HTTPTimeout maps
// directly. A zero/negative timeout falls back to 10s so a misconfig
// never leads to a goroutine hang inside the worker pool.
func New(cfg config.Telegram) *Client {
	timeout := cfg.HTTPTimeout.Std()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		httpC: &http.Client{Timeout: timeout},
	}
}

// newWithBase is a test-only helper: it builds a Client that rewrites
// the host portion of any webhook URL it receives so an httptest
// server can capture the request without exposing apiBase on the
// public type.
func newWithBase(base string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &Client{
		httpC:   &http.Client{Timeout: timeout},
		apiBase: base,
	}
}

// sendReq is the JSON body Lark's webhook endpoint expects for a plain
// text message:
//
//	{"msg_type":"text","content":{"text":"hello"}}
type sendReq struct {
	MsgType string      `json:"msg_type"`
	Content sendContent `json:"content"`
}

type sendContent struct {
	Text string `json:"text"`
}

// sendResp mirrors the response envelope. A successful post yields
// {"StatusCode":0,"StatusMessage":"success"} on legacy webhooks and
// {"code":0,"msg":"success","data":{}} on the v2 endpoint. We accept
// either shape and treat any non-zero code (in EITHER spelling) as a
// failure so the worker classifies it correctly.
type sendResp struct {
	Code          int    `json:"code"`
	Msg           string `json:"msg"`
	StatusCode    int    `json:"StatusCode"`
	StatusMessage string `json:"StatusMessage"`
}

// Send posts a plain-text message to the Lark webhook identified by
// botToken. The chatID, parseMode arguments are accepted for Sender
// interface compatibility but are ignored — the chat is bound to the
// webhook URL itself, and Lark text messages are plain (no MarkdownV2
// concept). The returned msgID is always 0 because Lark custom bot
// webhooks do not echo a stable message identifier.
//
// Returns ErrBadWebhook when botToken does not match the canonical
// webhook URL shape; that error is a domain-level "permanent client"
// signal and the caller (worker) DLQs the row instead of retrying.
// Network failures and non-zero Lark codes come back as *APIError so
// the worker can branch on .Class the same way it already does for
// tg.APIError.
// buildLarkBody chooses between rich-content pass-through and the
// legacy text wrapper. If text is a JSON object whose top level
// declares "msg_type" (text / post / interactive / image / share_chat
// / etc.) we trust the operator and forward it raw; this is how
// Markdown-style templates compose Lark post / card payloads without
// the server needing a per-format renderer. Anything else — including
// JSON that does NOT declare msg_type — is wrapped as a plain text
// message, preserving the original v1 behaviour.
func buildLarkBody(text string) ([]byte, error) {
	if isLarkEnvelope(text) {
		return []byte(text), nil
	}
	return json.Marshal(sendReq{
		MsgType: "text",
		Content: sendContent{Text: text},
	})
}

// isLarkEnvelope reports whether s is a JSON object with a string
// "msg_type" field — the marker that the template author meant to
// hand-craft a Lark message envelope rather than send plain text.
// We intentionally do not validate the rest of the schema; Lark's
// own response surfaces shape errors via non-zero `code`, which the
// worker already treats as a permanent failure.
func isLarkEnvelope(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" || trimmed[0] != '{' {
		return false
	}
	var probe struct {
		MsgType string `json:"msg_type"`
	}
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return false
	}
	return probe.MsgType != ""
}

func (c *Client) Send(ctx context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	_ = chatID
	_ = parseMode
	if !webhookPattern.MatchString(botToken) {
		return 0, ErrBadWebhook
	}

	// Rich content pass-through: if the rendered template body is
	// already a Lark message envelope (its top-level JSON object
	// contains a "msg_type" key) we forward it verbatim instead of
	// wrapping it as plain text. Operators can compose post / image /
	// interactive cards by simply emitting the full JSON from a
	// template. Anything else stays a text message — the legacy code
	// path, fully backwards compatible.
	body, err := buildLarkBody(text)
	if err != nil {
		return 0, fmt.Errorf("lark: build body: %w", err)
	}

	url := botToken
	if c.apiBase != "" {
		// Test redirection: replace the canonical host with the
		// httptest server while preserving the path so the handler
		// still sees /open-apis/bot/v2/hook/<key>.
		url = c.apiBase + extractPath(botToken)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, fmt.Errorf("lark: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpC.Do(req)
	if err != nil {
		return 0, &APIError{
			Class:       Transient,
			Description: "http do: " + err.Error(),
		}
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "read body: " + readErr.Error(),
		}
	}

	if resp.StatusCode >= 500 {
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "lark 5xx: " + safeBodyExcerpt(respBody),
		}
	}
	if resp.StatusCode == 429 {
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "lark rate limited",
		}
	}
	if resp.StatusCode >= 400 {
		// 4xx other than 429: token / webhook is permanently wrong.
		return 0, &APIError{
			Class:       PermanentClient,
			Code:        resp.StatusCode,
			Description: "lark 4xx: " + safeBodyExcerpt(respBody),
		}
	}

	var env sendResp
	if err := json.Unmarshal(respBody, &env); err != nil {
		// 2xx with unparseable JSON: treat as transient so a one-off
		// gateway hiccup doesn't kill the row.
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "parse body: " + err.Error(),
		}
	}
	code := env.Code
	if code == 0 {
		code = env.StatusCode
	}
	if code != 0 {
		// Non-zero application-level code. Lark uses 9499 for "invalid
		// webhook token" and similar 4-digit codes for malformed
		// payloads — all permanent client errors from the worker's POV.
		return 0, &APIError{
			Class:       PermanentClient,
			Code:        code,
			Description: "lark code=" + fmt.Sprint(code) + " msg=" + firstNonEmpty(env.Msg, env.StatusMessage),
		}
	}
	return 0, nil
}

// Edit is the SenderWithOpts compatibility shim. Lark custom-bot
// webhooks have no editMessageText analogue, so per the L2 spec we
// silently fall back to a fresh Send: the new state lands as a brand-
// new message in the same group, which is the most useful degradation
// from the operator's POV (V7-2 state machine still surfaces the
// update; it just doesn't collapse in place).
//
// The messageID argument is ignored. parseMode is forwarded to Send
// only so the signature matches; Lark itself ignores it.
func (c *Client) Edit(ctx context.Context, botToken, chatID string, messageID int64, parseMode, text string) error {
	_ = messageID
	_, err := c.Send(ctx, botToken, chatID, parseMode, text)
	return err
}

// extractPath returns the path portion of a webhook URL (everything
// after the host). Used only when c.apiBase is non-empty (test mode).
// We avoid net/url here because the input has already passed the
// webhookPattern regex check, so a literal "/open-apis..." prefix is
// guaranteed.
func extractPath(webhookURL string) string {
	const host = "https://open.feishu.cn"
	if len(webhookURL) >= len(host) && webhookURL[:len(host)] == host {
		return webhookURL[len(host):]
	}
	return webhookURL
}

// safeBodyExcerpt trims and clips raw bodies for inclusion in error
// messages without leaking unbounded HTML from a misconfigured proxy.
func safeBodyExcerpt(b []byte) string {
	const max = 200
	if len(b) > max {
		b = b[:max]
	}
	return string(bytes.TrimSpace(b))
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// AsAPIError unwraps any error chain to extract a *APIError when
// present. Mirrors tg.AsAPIError so callers that want the same
// type-switch pattern across both platforms can do it cleanly.
func AsAPIError(err error) (*APIError, bool) {
	var e *APIError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
