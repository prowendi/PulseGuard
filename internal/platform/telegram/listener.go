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
	"github.com/wendi/pulseguard/internal/platform"
)

// ErrTokenInvalid signals Telegram returned 401 Unauthorized. The
// Manager logs and stops the listener; an operator must rotate the
// token before the bot listens again. It is aliased to
// platform.ErrTokenInvalid so the Manager (which lives in the parent
// package and cannot import this one without a cycle) can match it
// with errors.Is and route the bot through the 401-auto-disable hook.
var ErrTokenInvalid = platform.ErrTokenInvalid

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

// CommandCatalog enumerates the enabled custom commands that belong to
// this listener's bot. Used by the listener to advertise its slash
// menu via Telegram setMyCommands on startup and to power built-in
// helpers like /commands and /unsubscribe. List MUST only return
// enabled rows — the listener treats the result as the public catalog.
//
// botID is the PulseGuard DB primary key (bots.id), matching the same
// key the CommandResolver in cmdrun uses, NOT the numeric Telegram
// token prefix. Conflating the two silently empties the catalog.
type CommandCatalog interface {
	ListByBot(ctx context.Context, botID int64) ([]CommandSummary, error)
}

// CommandSummary is the listener-facing projection of a custom command.
// Only the public-safe fields ("/name" without leading slash + the user-
// visible description) cross the boundary; the underlying Starlark code
// stays in the runtime layer.
type CommandSummary struct {
	Name        string
	Description string
}

// SubscriberRemover deletes a (bot, chat, command-name) subscription
// row. Used by the listener's built-in /unsubscribe command so users
// can opt out of a custom command without involving the operator.
// Returns ErrNotFound when no row matches (the listener turns that
// into a friendly Chinese reply).
type SubscriberRemover interface {
	DeleteByChatAndCommand(ctx context.Context, botID int64, chatID, commandName string) error
}

// AlertAcker records that an operator acknowledged an alert via the
// listener's /ack <fingerprint> built-in. Insert returns
// ErrAckAlreadyExists when the (bot's tenant, fingerprint) pair has
// already been acked — the listener turns that into a friendly
// "已记录" reply rather than surfacing the SQL constraint failure.
//
// botID + chatID identify the source of the ack so the audit row
// carries enough breadcrumbs to answer "who acked this and where".
// The listener resolves tenant via the bot row before calling.
type AlertAcker interface {
	Insert(ctx context.Context, in AckInput) error
}

// SilenceManager exposes the narrow set of operations the V7-3
// /silence built-ins need. Implementations live in runtime/ and
// resolve the bot → tenant mapping before forwarding to the
// underlying domain.SilenceRepo.
//
// Insert: create a silence rule.
// List:   active silences for the bot's tenant.
// DeleteByPattern: drop every active silence whose pattern matches the
// supplied string (returns number affected so the listener can craft
// a useful reply).
type SilenceManager interface {
	Insert(ctx context.Context, in SilenceInsertInput) error
	List(ctx context.Context, botID int64) ([]SilenceSummary, error)
	DeleteByPattern(ctx context.Context, botID int64, pattern string) (int64, error)
}

// SilenceInsertInput is the listener → SilenceManager contract for
// /silence. Duration is parsed by the listener (time.ParseDuration)
// before the manager sees it so the manager can stamp ExpiresAt with
// the injected clock and stay deterministic in tests.
type SilenceInsertInput struct {
	BotID     int64
	ChatID    string
	Pattern   string
	Duration  time.Duration
	CreatedBy string
}

// SilenceSummary is the listener-facing projection of a domain.Silence.
// Pattern + ExpiresAt + CreatedBy are the operator-relevant fields;
// id is included so /silence_list output can pair with an explicit
// /unsilence_id flow later if needed.
type SilenceSummary struct {
	ID        int64
	Pattern   string
	CreatedBy string
	ExpiresAt time.Time
}

// AckInput is the listener → AlertAcker contract.
type AckInput struct {
	BotID       int64
	ChatID      string
	Fingerprint string
	AckedBy     string // Telegram @username, or "chat:<chat_id>" fallback
}

// ErrAckAlreadyExists signals the (tenant, fingerprint) ack row was
// already present. The listener treats this as a successful no-op
// with a distinct user reply.
var ErrAckAlreadyExists = errors.New("telegram: ack already exists")

// HealthHook is the listener → Manager.RecordX bridge for the V6-2
// in-memory health panel. Implementations are expected to be
// non-blocking and very cheap (map lookup + counter bump). The three
// callbacks fire in the listener's hot path:
//
//   - OnUpdate    after a successful non-empty getUpdates batch.
//   - OnDispatch  after a successful custom-command dispatch (NOT for
//                 built-ins like /start, /commands, /ack — those are
//                 onboarding/management, not "the bot is doing work").
//   - OnError     when any listener operation surfaces an error, with
//                 a short kind string so the recorder can prefix the
//                 message ("getUpdates: …", "sendMessage: …").
//
// Defined as plain function fields on Options instead of a struct
// interface so the platform package can inject hooks without the
// listener importing platform (which would re-introduce the cycle
// the ErrTokenInvalid aliasing was designed to avoid).
type HealthHook struct {
	OnUpdate   func(botID int64)
	OnDispatch func(botID int64)
	OnError    func(botID int64, kind string, err error)
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
	botID      int64 // parsed from token prefix (Telegram platform id);
	//                used for self-recognition in new_chat_members.
	dbBotID    int64 // PulseGuard internal bot.ID (DB primary key);
	//                used for dispatcher → CommandRepo.GetByBotAndName,
	//                whose SQL JOIN keys on bots.id (NOT on the
	//                telegram-side numeric prefix). The two namespaces
	//                MUST stay separated; conflating them silently
	//                breaks every custom-command dispatch (no row
	//                matches and the listener returns "skip").
	botName    string
	tenantID   int64
	logger     *slog.Logger
	dispatcher CommandDispatcher
	catalog    CommandCatalog    // optional: powers setMyCommands + /commands
	remover    SubscriberRemover // optional: powers /unsubscribe
	acker      AlertAcker        // optional: powers /ack
	silences   SilenceManager    // optional: powers /silence /silence_list /unsilence
	health     HealthHook        // optional: bumps Manager health counters
}

// Options bundles the optional knobs. apiBase defaults to
// https://api.telegram.org. http is allowed to be nil — a sane default
// client with a 30 s timeout (>longPollTimeoutSec) is built.
//
// Dispatcher, when non-nil, enables custom-command handling. When nil
// the listener only answers /start, /chatid, and the bot-joined-group
// event (legacy MVP behaviour).
//
// Catalog, when non-nil, is consulted exactly once on Run startup so
// the listener can publish its slash menu via setMyCommands. It also
// backs the built-in /commands helper.
//
// Remover, when non-nil, powers the built-in /unsubscribe command.
//
// Acker, when non-nil, powers the built-in /ack <fingerprint> command.
//
// Health, when its callbacks are non-nil, bridges hot-path events to
// the Manager's in-memory health panel (V6-2). Individual callbacks
// may be nil — the listener nil-checks each before invoking.
type Options struct {
	APIBase    string
	HTTP       *http.Client
	Logger     *slog.Logger
	Dispatcher CommandDispatcher
	Catalog    CommandCatalog
	Remover    SubscriberRemover
	Acker      AlertAcker
	Silences   SilenceManager
	Health     HealthHook
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
		dbBotID:    bot.ID,
		botName:    bot.Name,
		tenantID:   bot.TenantID,
		logger:     logger,
		dispatcher: opts.Dispatcher,
		catalog:    opts.Catalog,
		remover:    opts.Remover,
		acker:      opts.Acker,
		silences:   opts.Silences,
		health:     opts.Health,
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
	l.logger.Info("telegram: listener started",
		"bot_id", l.botID,
		"tenant_id", l.tenantID,
		"dispatcher", l.dispatcher != nil)
	defer l.logger.Info("telegram: listener stopped", "bot_id", l.botID)

	// Publish the slash menu on startup so users see the tenant's
	// custom commands in the Telegram UI command picker. Failures here
	// are advisory only — they must NEVER abort the listener loop. A
	// short context isolates the HTTP call from getUpdates cancellation
	// semantics; even a slow Telegram backend just logs a warn.
	if l.catalog != nil {
		setCtx, setCancel := context.WithTimeout(ctx, 10*time.Second)
		if err := l.publishCommands(setCtx); err != nil {
			l.logger.Warn("telegram: setMyCommands failed",
				"bot_id", l.botID,
				"tenant_id", l.tenantID,
				"err", err.Error())
		}
		setCancel()
	}

	var offset int64
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		updates, err := l.getUpdates(ctx, offset)
		if err != nil {
			if errors.Is(err, ErrTokenInvalid) {
				l.recordError("getUpdates", err)
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
			l.recordError("getUpdates", err)
			if !sleepCtx(ctx, delay) {
				return nil
			}
			continue
		}

		// Health: a non-empty update batch is proof of liveness.
		// Empty long-poll rounds (the steady-state idle case) do not
		// count — they would otherwise flap the "last seen" clock every
		// 25 s and obscure a truly silent bot.
		if len(updates) > 0 {
			l.recordUpdate()
		}

		for _, u := range updates {
			l.handle(ctx, u)
			if u.UpdateID >= offset {
				offset = u.UpdateID + 1
			}
		}
	}
}

// publishCommands calls Telegram's setMyCommands so the bot's slash
// menu in the Telegram app surfaces every enabled custom command for
// this tenant. The catalog is consulted via the bot's DB primary key
// (NOT the Telegram numeric token-prefix id) — same convention as the
// dispatcher.
//
// Telegram's API expects a "/cmd" name WITHOUT the leading slash and
// a non-empty description. Commands stored as "/echo" in the catalog
// surface as "echo" here; descriptions falling back to "(no description)"
// because TG rejects empty strings outright.
//
// Errors are non-fatal — callers log and continue. A 200 with ok=false
// is treated as an error so a Telegram-side parse failure surfaces in
// the operator log rather than silently dropping the menu.
func (l *Listener) publishCommands(ctx context.Context) error {
	cmds, err := l.catalog.ListByBot(ctx, l.dbBotID)
	if err != nil {
		return fmt.Errorf("catalog: %w", err)
	}
	type wireCmd struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	wire := make([]wireCmd, 0, len(cmds))
	for _, c := range cmds {
		name := strings.TrimPrefix(strings.TrimSpace(c.Name), "/")
		if name == "" {
			continue
		}
		desc := strings.TrimSpace(c.Description)
		if desc == "" {
			desc = "(no description)"
		}
		wire = append(wire, wireCmd{Command: name, Description: desc})
	}
	body, err := json.Marshal(map[string]any{"commands": wire})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	u := l.apiBase + "/bot" + url.PathEscape(l.botToken) + "/setMyCommands"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.httpC.Do(req)
	if err != nil {
		return fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncForLog(respBody))
	}
	// Best-effort body parse: any ok=false surfaces as an error so the
	// operator can fix a malformed description without scanning HTTP 200s.
	var env struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(respBody, &env); err == nil && !env.OK {
		return fmt.Errorf("ok=false: %s", env.Description)
	}
	l.logger.Info("telegram: setMyCommands published",
		"bot_id", l.botID, "count", len(wire))
	return nil
}

// handle inspects a single update and replies if the user typed an
// onboarding command, a custom command, or the bot was added to a
// group. V7-1 adds a callback_query path so inline-keyboard taps
// (currently the "ack:<fingerprint>" convention) flow through the
// same handler.
func (l *Listener) handle(ctx context.Context, u update) {
	if u.CallbackQuery != nil {
		l.handleCallback(ctx, u.CallbackQuery)
		return
	}
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
	case "/commands":
		l.handleListCommands(ctx, msg.Chat.ID)
		return
	case "/unsubscribe":
		var arg string
		if len(tokens) > 1 {
			arg = tokens[1]
		}
		l.handleUnsubscribe(ctx, msg.Chat.ID, arg)
		return
	case "/ack":
		var fp string
		if len(tokens) > 1 {
			fp = tokens[1]
		}
		l.handleAck(ctx, msg.Chat.ID, fp, msg.From)
		return
	case "/silence":
		var pattern, dur string
		if len(tokens) > 1 {
			pattern = tokens[1]
		}
		if len(tokens) > 2 {
			dur = tokens[2]
		}
		l.handleSilence(ctx, msg.Chat.ID, pattern, dur, msg.From)
		return
	case "/silence_list":
		l.handleSilenceList(ctx, msg.Chat.ID)
		return
	case "/unsilence":
		var pattern string
		if len(tokens) > 1 {
			pattern = tokens[1]
		}
		l.handleUnsilence(ctx, msg.Chat.ID, pattern)
		return
	}

	// Anything else starting with "/" is a candidate for the
	// custom-command dispatcher. Stay silent if no dispatcher is
	// wired or the dispatcher returns ErrDispatchSkip.
	if !strings.HasPrefix(cmd, "/") {
		return
	}
	if l.dispatcher == nil {
		l.logger.Warn("telegram: custom command received but dispatcher not wired",
			"bot_id", l.botID, "tenant_id", l.tenantID,
			"cmd", cmd, "chat_id", msg.Chat.ID)
		return
	}
	name := strings.TrimPrefix(cmd, "/")
	args := []string{}
	if len(tokens) > 1 {
		args = append(args, tokens[1:]...)
	}
	l.logger.Info("telegram: dispatching custom command",
		"bot_id", l.botID, "tenant_id", l.tenantID,
		"name", name, "chat_id", msg.Chat.ID, "arg_count", len(args))
	out, err := l.dispatcher.Dispatch(ctx, DispatchInput{
		BotID:  l.dbBotID,
		ChatID: msg.Chat.ID,
		Name:   name,
		Args:   args,
	})
	if err != nil {
		if errors.Is(err, ErrDispatchSkip) {
			l.logger.Info("telegram: command not registered (skip)",
				"bot_id", l.botID, "tenant_id", l.tenantID,
				"name", name)
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
		l.logger.Info("telegram: dispatch returned empty text (no reply sent)",
			"bot_id", l.botID, "name", name)
		l.recordDispatch()
		return
	}
	l.logger.Info("telegram: dispatched ok",
		"bot_id", l.botID, "name", name, "reply_len", len(out.Text))
	l.recordDispatch()
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

// handleListCommands replies with a human-readable list of every
// enabled custom command the catalog exposes for this bot's tenant.
// Used when the user types /commands. When the catalog is not wired
// (legacy MVP harness) the listener stays silent — the user typed an
// unknown slash command and we route it through the normal "unknown"
// path by returning early so the dispatcher branch still runs. In the
// wired path we own the response and never fall through.
func (l *Listener) handleListCommands(ctx context.Context, chatID int64) {
	if l.catalog == nil {
		// No catalog wired — degrade to silence (mirrors the legacy
		// behaviour for unknown slash commands without a dispatcher).
		l.logger.Info("telegram: /commands received but no catalog wired",
			"bot_id", l.botID, "tenant_id", l.tenantID)
		return
	}
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmds, err := l.catalog.ListByBot(listCtx, l.dbBotID)
	if err != nil {
		l.logger.Warn("telegram: /commands catalog failed",
			"bot_id", l.botID, "tenant_id", l.tenantID, "err", err.Error())
		l.replyText(ctx, chatID, "查询命令列表失败")
		return
	}
	if len(cmds) == 0 {
		l.replyText(ctx, chatID, "暂无可用命令")
		return
	}
	var b strings.Builder
	b.WriteString("可用命令：\n")
	for _, c := range cmds {
		name := "/" + strings.TrimPrefix(strings.TrimSpace(c.Name), "/")
		if name == "/" {
			continue
		}
		b.WriteString(name)
		desc := strings.TrimSpace(c.Description)
		if desc != "" {
			b.WriteString(" — ")
			b.WriteString(desc)
		}
		b.WriteByte('\n')
	}
	l.replyText(ctx, chatID, strings.TrimRight(b.String(), "\n"))
}

// handleUnsubscribe processes "/unsubscribe [name]". With no argument
// the listener cannot guess which command to drop — we reply with the
// usage hint. With an argument we ask the remover to delete the
// (bot, chat, name) row; ErrNotFound surfaces as "未订阅" so the user
// learns nothing about other tenants' commands.
//
// chatID is the int64 from the Telegram payload, but the subscriber
// row stores it as a string (see SubscriberRepo.Upsert) so we format
// it explicitly here.
func (l *Listener) handleUnsubscribe(ctx context.Context, chatID int64, name string) {
	name = strings.TrimSpace(strings.TrimPrefix(name, "/"))
	if name == "" {
		l.replyText(ctx, chatID, "用法：/unsubscribe <命令名>。\n输入 /commands 查看可用命令。")
		return
	}
	if l.remover == nil {
		l.logger.Info("telegram: /unsubscribe received but remover not wired",
			"bot_id", l.botID, "tenant_id", l.tenantID, "name", name)
		return
	}
	delCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	chatStr := strconv.FormatInt(chatID, 10)
	err := l.remover.DeleteByChatAndCommand(delCtx, l.dbBotID, chatStr, name)
	switch {
	case err == nil:
		l.replyText(ctx, chatID, "已取消订阅 /"+name)
	case errors.Is(err, domain.ErrNotFound):
		l.replyText(ctx, chatID, "未订阅 /"+name)
	default:
		l.logger.Warn("telegram: /unsubscribe failed",
			"bot_id", l.botID, "tenant_id", l.tenantID,
			"name", name, "chat_id", chatID, "err", err.Error())
		l.replyText(ctx, chatID, "取消订阅失败")
	}
}

// handleAck records an operator acknowledgement against the supplied
// fingerprint. Reply variants:
//
//   - no argument           → usage hint
//   - acker not wired       → silent (legacy harness)
//   - success               → "已 ACK: <fp> by @<user>"
//   - duplicate (ErrAck…)   → "已记录"  (idempotent friendly path)
//   - other failure         → "ACK 失败"
//
// The "ackedBy" label is preferred in this order:
//   1. @username (Telegram identity, most useful for audit)
//   2. first + last name fallback
//   3. "chat:<chat_id>" terminal fallback so the audit row never
//      records the literal string "unknown"
func (l *Listener) handleAck(ctx context.Context, chatID int64, fp string, from *chatUser) {
	fp = strings.TrimSpace(fp)
	if fp == "" {
		l.replyText(ctx, chatID, "用法：/ack <fingerprint>")
		return
	}
	if l.acker == nil {
		l.logger.Info("telegram: /ack received but acker not wired",
			"bot_id", l.botID, "tenant_id", l.tenantID, "fp", fp)
		return
	}
	ackedBy := ackedByLabel(from, chatID)
	ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	chatStr := strconv.FormatInt(chatID, 10)
	err := l.acker.Insert(ackCtx, AckInput{
		BotID:       l.dbBotID,
		ChatID:      chatStr,
		Fingerprint: fp,
		AckedBy:     ackedBy,
	})
	switch {
	case err == nil:
		l.replyText(ctx, chatID, "已 ACK: "+fp+" by "+ackedBy)
	case errors.Is(err, ErrAckAlreadyExists):
		l.replyText(ctx, chatID, "已记录: "+fp)
	default:
		l.logger.Warn("telegram: /ack failed",
			"bot_id", l.botID, "tenant_id", l.tenantID,
			"fp", fp, "chat_id", chatID, "err", err.Error())
		l.replyText(ctx, chatID, "ACK 失败")
	}
}

// ackedByLabel picks a friendly identity for the ack audit row.
func ackedByLabel(from *chatUser, chatID int64) string {
	if from != nil {
		if u := strings.TrimSpace(from.Username); u != "" {
			return "@" + u
		}
		name := strings.TrimSpace(from.FirstName + " " + from.LastName)
		if name != "" {
			return name
		}
	}
	return "chat:" + strconv.FormatInt(chatID, 10)
}

// handleSilence processes "/silence <pattern> <duration>". The
// duration is parsed via time.ParseDuration so "1h", "30m", "2h30m"
// are all valid. Replies cover the common operator typos so the user
// learns what shape the command wants without consulting docs.
func (l *Listener) handleSilence(ctx context.Context, chatID int64, pattern, dur string, from *chatUser) {
	pattern = strings.TrimSpace(pattern)
	dur = strings.TrimSpace(dur)
	if pattern == "" || dur == "" {
		l.replyText(ctx, chatID, "用法：/silence <pattern> <duration>\n例如：/silence db01 2h")
		return
	}
	if l.silences == nil {
		l.logger.Info("telegram: /silence received but silences not wired",
			"bot_id", l.botID, "tenant_id", l.tenantID, "pattern", pattern)
		return
	}
	parsed, err := time.ParseDuration(dur)
	if err != nil {
		l.replyText(ctx, chatID, "无法解析时长 "+dur+"（示例：1h, 30m, 2h30m）")
		return
	}
	if parsed <= 0 {
		l.replyText(ctx, chatID, "时长必须为正值")
		return
	}
	createdBy := ackedByLabel(from, chatID)
	silenceCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	err = l.silences.Insert(silenceCtx, SilenceInsertInput{
		BotID:     l.dbBotID,
		ChatID:    strconv.FormatInt(chatID, 10),
		Pattern:   pattern,
		Duration:  parsed,
		CreatedBy: createdBy,
	})
	if err != nil {
		l.logger.Warn("telegram: /silence insert failed",
			"bot_id", l.botID, "tenant_id", l.tenantID,
			"pattern", pattern, "duration", parsed.String(), "err", err.Error())
		l.replyText(ctx, chatID, "添加静默失败")
		return
	}
	l.replyText(ctx, chatID, "已静默 "+pattern+" "+parsed.String())
}

// handleSilenceList replies with the active silences for the bot's
// tenant. Empty manager → silent (consistent with /commands).
func (l *Listener) handleSilenceList(ctx context.Context, chatID int64) {
	if l.silences == nil {
		l.logger.Info("telegram: /silence_list received but silences not wired",
			"bot_id", l.botID, "tenant_id", l.tenantID)
		return
	}
	listCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	rows, err := l.silences.List(listCtx, l.dbBotID)
	if err != nil {
		l.logger.Warn("telegram: /silence_list failed",
			"bot_id", l.botID, "tenant_id", l.tenantID, "err", err.Error())
		l.replyText(ctx, chatID, "查询静默列表失败")
		return
	}
	if len(rows) == 0 {
		l.replyText(ctx, chatID, "暂无活跃静默")
		return
	}
	var b strings.Builder
	b.WriteString("活跃静默：\n")
	for _, s := range rows {
		b.WriteString(s.Pattern)
		b.WriteString(" — 截止 ")
		b.WriteString(s.ExpiresAt.UTC().Format("2006-01-02 15:04:05"))
		b.WriteString(" UTC by ")
		b.WriteString(s.CreatedBy)
		b.WriteByte('\n')
	}
	l.replyText(ctx, chatID, strings.TrimRight(b.String(), "\n"))
}

// handleUnsilence drops every silence whose pattern matches the user's
// argument exactly. The reply names the affected count so the user
// learns whether their typo collapsed to a no-op.
func (l *Listener) handleUnsilence(ctx context.Context, chatID int64, pattern string) {
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		l.replyText(ctx, chatID, "用法：/unsilence <pattern>")
		return
	}
	if l.silences == nil {
		l.logger.Info("telegram: /unsilence received but silences not wired",
			"bot_id", l.botID, "tenant_id", l.tenantID, "pattern", pattern)
		return
	}
	delCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := l.silences.DeleteByPattern(delCtx, l.dbBotID, pattern)
	if err != nil {
		l.logger.Warn("telegram: /unsilence failed",
			"bot_id", l.botID, "tenant_id", l.tenantID,
			"pattern", pattern, "err", err.Error())
		l.replyText(ctx, chatID, "取消静默失败")
		return
	}
	if n == 0 {
		l.replyText(ctx, chatID, "未找到匹配的静默：" + pattern)
		return
	}
	l.replyText(ctx, chatID, fmt.Sprintf("已取消 %d 条静默：%s", n, pattern))
}

// recordUpdate / recordDispatch / recordError are nil-safe wrappers
// around the optional HealthHook callbacks injected via Options.
// Hot-path helpers — keep them inline-able.
func (l *Listener) recordUpdate() {
	if l.health.OnUpdate != nil {
		l.health.OnUpdate(l.dbBotID)
	}
}

func (l *Listener) recordDispatch() {
	if l.health.OnDispatch != nil {
		l.health.OnDispatch(l.dbBotID)
	}
}

func (l *Listener) recordError(kind string, err error) {
	if l.health.OnError != nil && err != nil {
		l.health.OnError(l.dbBotID, kind, err)
	}
}

// handleCallback routes a Telegram callback_query update. V7-1 only
// understands the "ack:<fingerprint>" data convention: when the user
// taps the ACK inline button on an alert message we
//   1. clear the loading spinner via answerCallbackQuery (always, so
//      Telegram does not leave the button in "..." for 15 s);
//   2. record the ack via the AlertAcker (if wired);
//   3. echo "@user 已 ACK" into the original message via
//      editMessageText so every chat participant sees the operator's
//      claim immediately.
//
// Any other data string is silently ignored — Telegram still requires
// us to call answerCallbackQuery, so we always close the spinner
// even on the no-op path.
func (l *Listener) handleCallback(ctx context.Context, cq *callbackQuery) {
	if cq == nil {
		return
	}
	defer l.answerCallback(ctx, cq.ID, "")

	data := strings.TrimSpace(cq.Data)
	if !strings.HasPrefix(data, "ack:") {
		l.logger.Info("telegram: callback ignored (unknown prefix)",
			"bot_id", l.botID, "data_len", len(data))
		return
	}
	fp := strings.TrimSpace(strings.TrimPrefix(data, "ack:"))
	if fp == "" {
		return
	}
	if l.acker == nil {
		l.logger.Info("telegram: callback ack received but acker not wired",
			"bot_id", l.botID, "fp", fp)
		return
	}

	var chatID int64
	if cq.Message != nil {
		chatID = cq.Message.Chat.ID
	}
	ackedBy := ackedByLabel(cq.From, chatID)
	ackCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	insertErr := l.acker.Insert(ackCtx, AckInput{
		BotID:       l.dbBotID,
		ChatID:      strconv.FormatInt(chatID, 10),
		Fingerprint: fp,
		AckedBy:     ackedBy,
	})
	switch {
	case insertErr == nil, errors.Is(insertErr, ErrAckAlreadyExists):
		// fresh ack OR duplicate — both proceed to the visual echo so
		// the user gets attribution feedback either way.
	default:
		l.logger.Warn("telegram: callback ack insert failed",
			"bot_id", l.botID, "fp", fp, "err", insertErr.Error())
		return
	}

	if cq.Message == nil || cq.Message.MessageID == 0 {
		return
	}
	original := cq.Message.Text
	newText := original
	if newText == "" {
		newText = "Alert"
	}
	suffix := "\n\n" + ackedBy + " 已 ACK"
	// Idempotent guard: if the suffix is already present we leave the
	// body untouched — editing with the same text triggers Telegram's
	// "message is not modified" 400 (which editMessageText below
	// swallows anyway), but skipping the round-trip is cheaper.
	if !strings.Contains(newText, suffix) {
		newText = newText + suffix
	}
	editCtx, editCancel := context.WithTimeout(ctx, 5*time.Second)
	defer editCancel()
	if err := l.editMessageText(editCtx, chatID, cq.Message.MessageID, newText); err != nil {
		l.logger.Warn("telegram: callback edit failed",
			"bot_id", l.botID, "fp", fp, "err", err.Error())
	}
}

// answerCallback closes the loading spinner on the user's Telegram
// client. Errors are logged but never propagate — the ack itself was
// the load-bearing side effect.
func (l *Listener) answerCallback(ctx context.Context, callbackID, text string) {
	if callbackID == "" {
		return
	}
	payload := map[string]any{"callback_query_id": callbackID}
	if text != "" {
		payload["text"] = text
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	u := l.apiBase + "/bot" + url.PathEscape(l.botToken) + "/answerCallbackQuery"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.httpC.Do(req)
	if err != nil {
		l.logger.Info("telegram: answerCallbackQuery transport failed",
			"bot_id", l.botID, "err", err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		bs, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		l.logger.Info("telegram: answerCallbackQuery non-2xx",
			"bot_id", l.botID, "status", resp.StatusCode, "body", string(bs))
	}
}

// editMessageText rewrites the text of a previously sent Telegram
// message. V7-1 uses this to echo "@user 已 ACK" into the alert body
// without losing the original inline_keyboard (Telegram preserves the
// existing reply_markup when the field is omitted).
//
// Telegram's "Bad Request: message is not modified" 400 is treated as
// a silent success — the desired end state is already in place.
func (l *Listener) editMessageText(ctx context.Context, chatID, messageID int64, text string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":    chatID,
		"message_id": messageID,
		"text":       text,
	})
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	u := l.apiBase + "/bot" + url.PathEscape(l.botToken) + "/editMessageText"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := l.httpC.Do(req)
	if err != nil {
		return fmt.Errorf("transport: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		bs, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if strings.Contains(string(bs), "not modified") {
			return nil
		}
		return fmt.Errorf("status %d: %s", resp.StatusCode, truncForLog(bs))
	}
	return nil
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
	UpdateID      int64          `json:"update_id"`
	Message       *message       `json:"message,omitempty"`
	CallbackQuery *callbackQuery `json:"callback_query,omitempty"`
}

// callbackQuery captures the bare minimum fields V7-1 needs to honour
// an inline-keyboard tap. ID identifies the query so we can clear the
// "..." loading spinner in the user's Telegram client via
// answerCallbackQuery. Data carries the button's callback_data string
// — we use the "ack:<fingerprint>" convention so the listener can
// route to the alert_acks insert + editMessageText echo.
//
// Message is the original message the button was attached to; the
// listener uses Message.Chat.ID + Message.MessageID to drive the
// editMessageText that prefixes "@user 已 ACK" to the alert body.
type callbackQuery struct {
	ID      string    `json:"id"`
	From    *chatUser `json:"from,omitempty"`
	Message *message  `json:"message,omitempty"`
	Data    string    `json:"data,omitempty"`
}

type message struct {
	Chat           chat       `json:"chat"`
	MessageID      int64      `json:"message_id,omitempty"`
	Text           string     `json:"text,omitempty"`
	From           *chatUser  `json:"from,omitempty"`
	NewChatMembers []chatUser `json:"new_chat_members,omitempty"`
}

type chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type,omitempty"`
}

type chatUser struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	Username  string `json:"username,omitempty"`
	FirstName string `json:"first_name,omitempty"`
	LastName  string `json:"last_name,omitempty"`
}

// getUpdates issues a long-poll request. The "allowed_updates" filter
// restricts Telegram's push to the two update kinds we actually
// handle: text messages (onboarding + custom commands + V7 built-ins)
// and callback queries (V7-1 inline-keyboard ACK button).
func (l *Listener) getUpdates(ctx context.Context, offset int64) ([]update, error) {
	q := url.Values{}
	q.Set("offset", strconv.FormatInt(offset, 10))
	q.Set("timeout", strconv.Itoa(longPollTimeoutSec))
	q.Set("allowed_updates", `["message","callback_query"]`)
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
