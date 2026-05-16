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

	"github.com/wendi/pulseguard/internal/config"
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

// sendMessageReq is the JSON body for /sendMessage.
type sendMessageReq struct {
	ChatID    string `json:"chat_id"`
	Text      string `json:"text"`
	ParseMode string `json:"parse_mode,omitempty"`
}

// sendMessageResp is the success envelope returned by Telegram.
type sendMessageResp struct {
	OK     bool `json:"ok"`
	Result struct {
		MessageID int64 `json:"message_id"`
	} `json:"result"`
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
