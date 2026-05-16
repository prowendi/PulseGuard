// Package telegram implements the Telegram getUpdates long-poll loop
// PulseGuard runs per Telegram bot. The Listener replies with the
// current chat_id whenever a user types /start or /chatid in a private
// chat, or when the bot is added to a group — solving the "user does
// not know their chat_id" onboarding paper-cut.
//
// Beyond onboarding, the Listener also dispatches per-tenant custom
// commands defined by users via a Starlark script: each `/name args…`
// message resolves through CommandDispatcher and the result is sent
// back to the originating chat.
//
// The Listener is intentionally narrow: it does not consume Telegram
// "edited_message", inline queries, or callback queries. allowed_updates
// is locked to ["message"] so Telegram's backend filters everything else
// before we even read it. Outbound pushes still go through internal/tg.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// ErrTokenInvalid signals Telegram returned 401 Unauthorized. The
// Manager logs and stops the listener; an operator must rotate the
// token before the bot listens again.
var ErrTokenInvalid = errors.New("telegram: bot token invalid (401)")

// CommandDispatcher resolves and executes user-defined Starlark
// commands. The Telegram listener calls Dispatch when it sees a
// "/name [args...]" message that is NOT one of the built-in commands
// (/start, /chatid). Dispatch returns the rendered text the listener
// should send back to the originating chat, or DispatchSkip when the
// command is unknown/disabled (the listener stays silent).
//
// Implementations live in the runtime/web layer so the listener can
// stay focused on Telegram concerns.
type CommandDispatcher interface {
	// Dispatch is invoked with the bot id (so the dispatcher can scope
	// to a tenant via the bots table), the chat that triggered it (used
	// to record subscribers and as the reply target), the command name
	// (already stripped of leading "/" — implementations re-add if they
	// need exact match), and the remaining tokens.
	//
	// A non-nil error indicates a runtime failure the caller should
	// surface to the user with a friendly message. ErrDispatchSkip
	// signals "no such command; stay silent".
	Dispatch(ctx context.Context, in DispatchInput) (DispatchOutput, error)
}

// DispatchInput is the listener → dispatcher contract.
type DispatchInput struct {
	BotID  int64
	ChatID int64
	// Name is the slash-command name WITHOUT the leading "/", and
	// WITHOUT any "@botname" suffix (the listener strips both).
	Name string
	Args []string
}

// DispatchOutput is the dispatcher → listener reply contract. Text is
// rendered into the originating chat as a plain Telegram message.
type DispatchOutput struct {
	Text string
}

// ErrDispatchSkip tells the listener the command was unknown or
// disabled and no reply should be sent.
var ErrDispatchSkip = errors.New("telegram: dispatch skip (unknown command)")

// long-poll constants. The 25 s timeout balances Telegram's 50 s upper
// bound against our shutdown deadline (15 s graceful + retry slack).
const (
	longPollTimeoutSec = 25
	pollErrorBackoff   = 5 * time.Second
)

// replyMessage is the message body sent back to a chat that contacted
// the bot via /start, /chatid, or by adding the bot to a group.
const replyTemplate = "PulseGuard 推送 bot 已接入。\n\n" +
	"Chat ID: %s\n\n" +
	"将以上 Chat ID 填入 PulseGuard 通道配置的 chat_id 字段，\n" +
	"告警将发送到本对话。"

// Listener long-polls getUpdates and replies to onboarding events.
//
// One Listener per bot per process. Listener is not safe for concurrent
// Run calls; the platform.Manager guarantees a single Run per Listener.
type Listener struct {
	httpC      *http.Client
	apiBase    string
	botToken   string
	botID      int64 // parsed from token prefix
	botName    string
	tenantID   int64
	logger     *slog.Logger
	dispatcher CommandDispatcher
}

// Options bundles the optional knobs. apiBase defaults to
// https://api.telegram.org. http is allowed to be nil — a sane default
// client with a 30 s timeout (>longPollTimeoutSec) is built.
//
// Dispatcher, when non-nil, enables custom-command handling. When nil
// the listener only answers /start, /chatid, and the bot-joined-group
// event (legacy MVP behaviour).
type Options struct {
	APIBase    string
	HTTP       *http.Client
	Logger     *slog.Logger
	Dispatcher CommandDispatcher
}

// New constructs a Listener for the supplied bot. The bot's BotToken
// must already be in the "<bot_id>:<secret>" shape — the bot_id prefix
// is what we use to recognise self-joins to groups.
func New(bot *domain.Bot, opts Options) (*Listener, error) {
	if bot == nil {
		return nil, errors.New("telegram: bot is nil")
	}
	if bot.BotToken == "" {
		return nil, errors.New("telegram: bot token is empty")
	}
	id, err := parseBotID(bot.BotToken)
	if err != nil {
		return nil, fmt.Errorf("telegram: %w", err)
	}
	base := opts.APIBase
	if base == "" {
		base = "https://api.telegram.org"
	}
	base = strings.TrimRight(base, "/")

	httpC := opts.HTTP
	if httpC == nil {
		// longPollTimeoutSec (25) + headroom for body read + dial.
		httpC = &http.Client{Timeout: time.Duration(longPollTimeoutSec+10) * time.Second}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Listener{
		httpC:      httpC,
		apiBase:    base,
		botToken:   bot.BotToken,
		botID:      id,
		botName:    bot.Name,
		tenantID:   bot.TenantID,
		logger:     logger,
		dispatcher: opts.Dispatcher,
	}, nil
}

// parseBotID extracts the numeric prefix from "<id>:<secret>". Returns
// 0 (and an error) for any token that does not follow the documented
// Telegram convention.
func parseBotID(token string) (int64, error) {
	idx := strings.IndexByte(token, ':')
	if idx <= 0 {
		return 0, errors.New("bot token missing ':' separator")
	}
	id, err := strconv.ParseInt(token[:idx], 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("bot token prefix is not a positive int64: %w", err)
	}
	return id, nil
}

// Run drains updates until ctx is cancelled or a permanent failure
// occurs. The expected terminal paths are:
//   - ctx cancel -> returns nil (or ctx.Err())
//   - HTTP 401   -> returns ErrTokenInvalid
//   - other err  -> log + sleep 5s + retry indefinitely
func (l *Listener) Run(ctx context.Context) error {
	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		updates, err := l.getUpdates(ctx, offset)
		if err != nil {
			if errors.Is(err, ErrTokenInvalid) {
				return err
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				if ctx.Err() != nil {
					return nil
				}
			}
			// Honour a 429 retry_after if present, else apply a flat
			// backoff. The error is wrapped with a *retryAfterErr when
			// Telegram dictated the wait.
			var ra *retryAfterErr
			delay := pollErrorBackoff
			if errors.As(err, &ra) && ra.after > 0 {
				delay = ra.after
			}
			l.logger.Warn("telegram: getUpdates failed; will retry",
				"bot_id", l.botID,
				"tenant_id", l.tenantID,
				"err", err.Error(),
				"sleep", delay.String())
			if !sleepCtx(ctx, delay) {
				return nil
			}
			continue
		}

		for _, u := range updates {
			l.handle(ctx, u)
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
		}
	}
}

// handle inspects a single update and replies if the user typed an
// onboarding command, a custom command, or the bot was added to a
// group.
func (l *Listener) handle(ctx context.Context, u update) {
	msg := u.Message
	if msg == nil {
		return
	}

	// Group-join: bot was added to a chat. new_chat_members carries
	// every user (including the bot itself). Reply only when the
	// addition includes our own bot id.
	if len(msg.NewChatMembers) > 0 {
		for _, m := range msg.NewChatMembers {
			if m.ID == l.botID && m.IsBot {
				l.replyChatID(ctx, msg.Chat.ID)
				return
			}
		}
	}

	// /start and /chatid commands. Strip "@botname" suffixes that
	// Telegram appends in group chats (e.g. "/start@my_bot").
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return
	}
	// Tokenise on whitespace so /查询 1 2 3 produces ["/查询", "1", "2", "3"].
	tokens := strings.Fields(text)
	if len(tokens) == 0 {
		return
	}
	cmd := tokens[0]
	if at := strings.IndexByte(cmd, '@'); at > 0 {
		cmd = cmd[:at]
	}
	switch cmd {
	case "/start", "/chatid":
		l.replyChatID(ctx, msg.Chat.ID)
		return
	}

	// Anything else starting with "/" is a candidate for the
	// custom-command dispatcher. Stay silent if no dispatcher is
	// wired or the dispatcher returns ErrDispatchSkip.
	if !strings.HasPrefix(cmd, "/") {
		return
	}
	if l.dispatcher == nil {
		return
	}
	name := strings.TrimPrefix(cmd, "/")
	args := []string{}
	if len(tokens) > 1 {
		args = append(args, tokens[1:]...)
	}
	out, err := l.dispatcher.Dispatch(ctx, DispatchInput{
		BotID:  l.botID,
		ChatID: msg.Chat.ID,
		Name:   name,
		Args:   args,
	})
	if err != nil {
		if errors.Is(err, ErrDispatchSkip) {
			return
		}
		// Friendly message; never echo raw Starlark stack traces.
		l.logger.Warn("telegram: command dispatch failed",
			"bot_id", l.botID, "tenant_id", l.tenantID,
			"name", name, "err", err.Error())
		l.replyText(ctx, msg.Chat.ID, friendlyDispatchError(err))
		return
	}
	if strings.TrimSpace(out.Text) == "" {
		return
	}
	l.replyText(ctx, msg.Chat.ID, out.Text)
}

// friendlyDispatchError maps a dispatcher error to a non-leaking user
// message. The dispatcher is expected to return wrapped sentinels
// from internal/scripting; if it cannot we fall back to a generic
// "命令执行失败".
func friendlyDispatchError(err error) string {
	switch {
	case errors.Is(err, ErrDispatchTimeout):
		return "命令执行超时"
	case errors.Is(err, ErrDispatchUnsafeHost):
		return "命令请求的地址不允许"
	case errors.Is(err, ErrDispatchUnsupportedScheme):
		return "命令请求的协议不允许"
	default:
		return "命令执行失败"
	}
}

// ErrDispatch* are sentinels the dispatcher should wrap so the listener
// can surface a tailored Chinese message without depending on the
// scripting package directly.
var (
	ErrDispatchTimeout           = errors.New("telegram: command timeout")
	ErrDispatchUnsafeHost        = errors.New("telegram: command unsafe host")
	ErrDispatchUnsupportedScheme = errors.New("telegram: command unsupported scheme")
)

// replyChatID best-effort sends the onboarding message containing the
// chat id. Errors are logged, never fatal — the listener should keep
// running even if a particular reply round-trip fails.
func (l *Listener) replyChatID(ctx context.Context, chatID int64) {
	l.replyText(ctx, chatID, fmt.Sprintf(replyTemplate, strconv.FormatInt(chatID, 10)))
}

// replyText is the underlying sendMessage helper. Errors are logged
// and never propagated so a transient TG hiccup cannot kill the loop.
func (l *Listener) replyText(ctx context.Context, chatID int64, text string) {
	body, err := json.Marshal(map[string]any{
		"chat_id": chatID,
		"text":    text,
	})
	if err != nil {
		l.logger.Warn("telegram: marshal reply failed",
			"bot_id", l.botID, "chat_id", chatID, "err", err.Error())
		return
	}
	u := l.apiBase + "/bot" + url.PathEscape(l.botToken) + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		l.logger.Warn("telegram: build sendMessage request failed",
			"bot_id", l.botID, "chat_id", chatID, "err", err.Error())
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.httpC.Do(req)
	if err != nil {
		l.logger.Warn("telegram: sendMessage transport failed",
			"bot_id", l.botID, "chat_id", chatID, "err", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Best-effort body capture for diagnostics.
		bs, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		l.logger.Warn("telegram: sendMessage non-2xx",
			"bot_id", l.botID,
			"chat_id", chatID,
			"status", resp.StatusCode,
			"body", string(bs))
		return
	}
	l.logger.Info("telegram: replied",
		"bot_id", l.botID, "chat_id", chatID)
}

// retryAfterErr wraps a transient error that carries a Telegram
// retry_after directive.
type retryAfterErr struct {
	after time.Duration
	cause error
}

func (e *retryAfterErr) Error() string {
	return fmt.Sprintf("retry after %s: %v", e.after, e.cause)
}
func (e *retryAfterErr) Unwrap() error { return e.cause }

// getUpdatesResponse is the JSON envelope returned by /getUpdates.
type getUpdatesResponse struct {
	OK          bool     `json:"ok"`
	Result      []update `json:"result"`
	ErrorCode   int      `json:"error_code"`
	Description string   `json:"description"`
	Parameters  *struct {
		RetryAfter int `json:"retry_after"`
	} `json:"parameters,omitempty"`
}

// update mirrors the fields of a Telegram Update we care about.
type update struct {
	UpdateID int64    `json:"update_id"`
	Message  *message `json:"message,omitempty"`
}

type message struct {
	Chat           chat       `json:"chat"`
	Text           string     `json:"text,omitempty"`
	NewChatMembers []chatUser `json:"new_chat_members,omitempty"`
}

type chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type,omitempty"`
}

type chatUser struct {
	ID    int64 `json:"id"`
	IsBot bool  `json:"is_bot"`
}

// getUpdates issues a long-poll request. The "allowed_updates" filter
// keeps Telegram from streaming us callback queries, edits, etc.
func (l *Listener) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	q := url.Values{}
	q.Set("offset", strconv.FormatInt(offset, 10))
	q.Set("timeout", strconv.Itoa(longPollTimeoutSec))
	q.Set("allowed_updates", `["message"]`)
	u := fmt.Sprintf("%s/bot%s/getUpdates?%s",
		l.apiBase, url.PathEscape(l.botToken), q.Encode())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("build getUpdates request: %w", err)
	}
	resp, err := l.httpC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getUpdates transport: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20)) // 4MB cap
	if err != nil {
		return nil, fmt.Errorf("getUpdates read body: %w", err)
	}

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, ErrTokenInvalid
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		ra := parseRetryAfter(body)
		return nil, &retryAfterErr{after: ra, cause: fmt.Errorf("429: %s", truncForLog(body))}
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("getUpdates status %d: %s", resp.StatusCode, truncForLog(body))
	}

	var env getUpdatesResponse
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("decode getUpdates: %w", err)
	}
	if !env.OK {
		// ok=false on a 2xx usually accompanies an explicit error_code.
		if env.ErrorCode == 401 {
			return nil, ErrTokenInvalid
		}
		return nil, fmt.Errorf("getUpdates ok=false code=%d desc=%q",
			env.ErrorCode, env.Description)
	}
	return env.Result, nil
}

func parseRetryAfter(body []byte) time.Duration {
	var env getUpdatesResponse
	if err := json.Unmarshal(body, &env); err == nil && env.Parameters != nil && env.Parameters.RetryAfter > 0 {
		return time.Duration(env.Parameters.RetryAfter) * time.Second
	}
	return 0
}

// truncForLog returns a short prefix of a Telegram response body so log
// lines stay scannable.
func truncForLog(b []byte) string {
	const max = 256
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "...(truncated)"
}

// sleepCtx blocks for d, returning false if ctx cancels first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
