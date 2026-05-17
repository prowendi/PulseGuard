package tg

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/prowendi/PulseGuard/internal/config"
)

// Client wraps a *http.Client targeting api.telegram.org. The api base is
// pluggable so tests can redirect to an httptest.Server.
type Client struct {
	httpC   *http.Client
	apiBase string
}

// New constructs a Client from config. cfg.APIBase defaults to
// https://api.telegram.org when empty; cfg.HTTPTimeout defaults to 10s.
func New(cfg config.Telegram) *Client {
	timeout := cfg.HTTPTimeout.Std()
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	base := cfg.APIBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimRight(base, "/")
	return &Client{
		httpC:   &http.Client{Timeout: timeout},
		apiBase: base,
	}
}

// sendMessageReq is the JSON body for /sendMessage. ReplyMarkup is
// optional; when set it carries an inline_keyboard payload built from
// InlineButton (see SendOpts.Buttons). The field is `any` so callers
// can keep the helper if Telegram ever extends the schema.
type sendMessageReq struct {
	ChatID      string `json:"chat_id"`
	Text        string `json:"text"`
	ParseMode   string `json:"parse_mode,omitempty"`
	ReplyMarkup any    `json:"reply_markup,omitempty"`
}

// sendMessageResp is the success envelope returned by Telegram.
type sendMessageResp struct {
	OK     bool `json:"ok"`
	Result struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
}

// editMessageTextReq is the JSON body for /editMessageText. The
// reply_markup field is intentionally omitted on every Edit call —
// V7-2 keeps the original inline_keyboard so operators can still ACK
// after a state-machine update; supplying nil here means "leave the
// existing markup as-is" per Telegram's contract.
type editMessageTextReq struct {
	ChatID    string `json:"chat_id"`
	MessageID int64  `json:"message_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

// editMessageTextResp envelope. Telegram returns the edited message
// shape on success; we only need OK.
type editMessageTextResp struct {
	OK bool `json:"ok"`
}

// InlineButton is the public shape callers pass when they want the
// outbound message to carry an inline keyboard. Exactly one of
// Callback or URL must be non-empty:
//   - Callback fires a Telegram callback_query update routed to the
//     listener (V7-1 uses the "ack:<fingerprint>" convention).
//   - URL opens a browser tab in the user's Telegram client.
// Both being set is rejected at the marshalling layer so a button
// never ambiguously routes between two transports.
type InlineButton struct {
	Text     string
	Callback string
	URL      string
}

// SendOpts bundles the optional knobs Send accepts. Kept as a struct
// so future additions (disable_notification, link_preview_options,
// reply_to_message_id, ...) do not break the call sites that only set
// Buttons today.
//
// Buttons, when non-empty, becomes a single-row inline_keyboard on the
// outbound sendMessage. Higher-level layers (V7-1 PushButton →
// pipeline) can serialise multi-row layouts here in the future; the
// MVP is one row to keep the rendered Telegram message scannable.
type SendOpts struct {
	Buttons []InlineButton
}

// buildInlineKeyboard turns a slice of InlineButton into the
// reply_markup payload Telegram expects. Returns nil when there are no
// buttons so json.Marshal omits the field via the `omitempty` tag.
//
// Each button must carry text plus exactly one of callback_data or
// url. We do NOT silently drop malformed entries because the caller
// authored them — a typo should surface as "no markup" so the operator
// notices the missing button in the rendered message rather than a
// silent partial.
func buildInlineKeyboard(buttons []InlineButton) any {
	if len(buttons) == 0 {
		return nil
	}
	type tgBtn struct {
		Text         string `json:"text"`
		CallbackData string `json:"callback_data,omitempty"`
		URL          string `json:"url,omitempty"`
	}
	row := make([]tgBtn, 0, len(buttons))
	for _, b := range buttons {
		text := strings.TrimSpace(b.Text)
		if text == "" {
			continue
		}
		cb := strings.TrimSpace(b.Callback)
		u := strings.TrimSpace(b.URL)
		if cb == "" && u == "" {
			continue
		}
		// Telegram caps callback_data at 64 bytes. Truncating silently
		// would cause the eventual callback_query to refer to a
		// fingerprint that no longer matches; clip explicitly so the
		// payload is at least self-consistent.
		if len(cb) > 64 {
			cb = cb[:64]
		}
		row = append(row, tgBtn{Text: text, CallbackData: cb, URL: u})
	}
	if len(row) == 0 {
		return nil
	}
	return map[string]any{
		"inline_keyboard": [][]tgBtn{row},
	}
}

// Send delivers text to chatID via the bot identified by botToken. The
// parseMode argument accepts "MarkdownV2", "HTML", "None", or empty —
// "None" / "" cause the parse_mode field to be omitted (Telegram rejects
// an empty parse_mode string).
//
// Returns (msgID, nil) on success or a typed *APIError on Telegram-side
// failure. Network/transport errors are wrapped as Transient *APIError so
// callers do not need to type-assert net errors separately.
func (c *Client) Send(ctx context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	return c.SendWithOpts(ctx, botToken, chatID, parseMode, text, SendOpts{})
}

// SendWithOpts is the variadic-extension variant of Send. The plain
// Send is preserved so it satisfies domain.Sender; callers needing
// V7-1 buttons (or future per-call knobs) call this method directly
// on *Client.
//
// On the happy path the bodies emitted by Send and SendWithOpts are
// identical, modulo the optional reply_markup field, so the
// integration tests covering plain Send continue to provide the
// regression net for the buttonless path.
func (c *Client) SendWithOpts(ctx context.Context, botToken, chatID, parseMode, text string, opts SendOpts) (int64, error) {
	if botToken == "" {
		return 0, fmt.Errorf("bot token is empty")
	}
	if chatID == "" {
		return 0, fmt.Errorf("chat_id is empty")
	}

	body := sendMessageReq{
		ChatID: chatID,
		Text:   text,
	}
	if pm := normalizeParseMode(parseMode); pm != "" {
		body.ParseMode = pm
	}
	if markup := buildInlineKeyboard(opts.Buttons); markup != nil {
		body.ReplyMarkup = markup
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return 0, fmt.Errorf("marshal send: %w", err)
	}

	u := fmt.Sprintf("%s/bot%s/sendMessage", c.apiBase, url.PathEscape(botToken))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
	if err != nil {
		return 0, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpC.Do(req)
	if err != nil {
		// Network failures (incl. context.DeadlineExceeded) -> Transient.
		return 0, &APIError{
			Class:       Transient,
			Code:        0,
			Description: fmt.Sprintf("http do: %s", err.Error()),
		}
	}
	defer resp.Body.Close()

	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: fmt.Sprintf("read body: %s", readErr.Error()),
		}
	}

	if classifyErr := Classify(resp.StatusCode, respBody); classifyErr != nil {
		return 0, classifyErr
	}

	// 2xx -> parse success envelope.
	var ok sendMessageResp
	if err := json.Unmarshal(respBody, &ok); err != nil {
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: fmt.Sprintf("parse success body: %s", err.Error()),
		}
	}
	if !ok.OK {
		return 0, &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "ok=false in 2xx body",
		}
	}
	return ok.Result.MessageID, nil
}

// Edit calls /editMessageText to replace the text of a previously sent
// message. Used by V7-2 (state-machine collapse) and V7-1 ACK echoes
// where the operator's "@user 已 ACK" prefix is appended to the
// original alert.
//
// Returns nil on success or a typed *APIError on Telegram-side failure
// (same classification rules as Send). Network errors come back as
// Transient *APIError. The "message is not modified" 400 from
// Telegram is treated as a silent success so duplicate edits do not
// surface as user-facing errors — the body in the channel is already
// what the caller wanted.
func (c *Client) Edit(ctx context.Context, botToken, chatID string, messageID int64, parseMode, text string) error {
	if botToken == "" {
		return fmt.Errorf("bot token is empty")
	}
	if chatID == "" {
		return fmt.Errorf("chat_id is empty")
	}
	if messageID == 0 {
		return fmt.Errorf("message_id is zero")
	}
	body := editMessageTextReq{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
	}
	if pm := normalizeParseMode(parseMode); pm != "" {
		body.ParseMode = pm
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal edit: %w", err)
	}
	u := fmt.Sprintf("%s/bot%s/editMessageText", c.apiBase, url.PathEscape(botToken))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpC.Do(req)
	if err != nil {
		return &APIError{
			Class:       Transient,
			Code:        0,
			Description: fmt.Sprintf("http do: %s", err.Error()),
		}
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: fmt.Sprintf("read body: %s", readErr.Error()),
		}
	}
	if classifyErr := Classify(resp.StatusCode, respBody); classifyErr != nil {
		// Telegram returns 400 with description "Bad Request: message is
		// not modified" when the edit text matches the current body
		// verbatim. Treat as silent success: the desired end-state is
		// already in place.
		if ae, ok := classifyErr.(*APIError); ok &&
			ae.Code == 400 && strings.Contains(ae.Description, "not modified") {
			return nil
		}
		return classifyErr
	}
	var env editMessageTextResp
	if err := json.Unmarshal(respBody, &env); err != nil {
		return &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: fmt.Sprintf("parse edit body: %s", err.Error()),
		}
	}
	if !env.OK {
		return &APIError{
			Class:       Transient,
			Code:        resp.StatusCode,
			Description: "ok=false in 2xx body",
		}
	}
	return nil
}

// normalizeParseMode returns the canonical Telegram value or "" when the
// caller asked for None.
func normalizeParseMode(pm string) string {
	switch strings.ToLower(strings.TrimSpace(pm)) {
	case "", "none":
		return ""
	case "markdownv2":
		return "MarkdownV2"
	case "html":
		return "HTML"
	default:
		return pm
	}
}

// AsAPIError unwraps any error chain to extract a *APIError when present.
// Returns (nil, false) when the error does not originate from this package.
func AsAPIError(err error) (*APIError, bool) {
	var e *APIError
	if errors.As(err, &e) {
		return e, true
	}
	return nil, false
}
