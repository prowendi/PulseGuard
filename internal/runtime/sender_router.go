// Package runtime — sender_router multiplexes the worker's single
// domain.Sender dependency across multiple chat-platform clients
// (Telegram + Lark / 飞书 webhook + Lark application bot today;
// pluggable for Discord / Slack / WeChat once their outbound clients
// land).
//
// Detection happens on the BotToken value the worker passes through
// from domain.Bot, NOT on bot.Platform. The router does not see the
// Bot row — it sees only the wire-level credentials the worker hands
// to Sender.Send. The chosen heuristic is a prefix test:
//
//   - "lark-app://" → Lark application bot → routed to lark.AppClient.
//     The store layer assembles this pseudo-URL from the app_id +
//     plaintext app_secret on read; the worker never knows it's
//     dealing with anything other than a token string.
//   - "https://open.feishu.cn/" → Lark webhook URL → routed to lark.Client.
//   - anything else (including the standard "<bot-id>:<secret>" shape)
//     → routed to the Telegram adapter.
//
// Detecting by token shape (instead of threading bot.Platform through
// the Sender interface) keeps the worker's call-site untouched —
// adding Lark is a wire-up change in runtime.RunWithDeps only. The
// price is a tiny string prefix check on the hot path, which is
// negligible next to the HTTP POST that follows.
package runtime

import (
	"context"
	"strings"

	"github.com/prowendi/PulseGuard/internal/domain"
	"github.com/prowendi/PulseGuard/internal/lark"
)

// larkURLPrefix is the canonical custom-bot webhook host. detectPlatform
// compares case-sensitively because URL hosts are not case-insensitive
// in the routing layer — operators who paste a mixed-case host get
// telegram routing (which then errors at the tg layer with a clean
// "bot token is not valid" instead of mis-routing to Lark).
const larkURLPrefix = "https://open.feishu.cn/"

// platformLarkApp is an internal marker returned by detectPlatform when
// the token shape matches the lark-app:// pseudo-URL the store layer
// emits for application-bot rows. Distinct from domain.PlatformLark
// (which covers both webhook and app rows) because the router needs
// to dispatch to lark.AppClient vs lark.Client based on the wire
// shape alone — bot.BotKind is not visible here.
const platformLarkApp = "lark-app"

// senderRouter is the multi-platform dispatcher. tg is the existing
// telegram adapter (SenderWithOpts capable); larkWebhook handles the
// Phase A custom-bot webhook; larkApp handles the Phase B application
// bot (OAuth2 + IM API). All three fields are required at construction
// time — there is no nil-fallback because a bot row carrying the
// "wrong" platform for a nil client should fail loudly rather than
// silently send nowhere.
type senderRouter struct {
	tg          domain.Sender
	larkWebhook *lark.Client
	larkApp     *lark.AppClient
}

// newSenderRouter wires the three underlying clients. Pass the existing
// *tgSenderAdapter (or any domain.Sender for tests) as tg, a
// *lark.Client as larkWebhook, and a *lark.AppClient as larkApp. The
// returned value satisfies BOTH domain.Sender AND domain.SenderWithOpts,
// so the worker's type assertion for SendWithOpts / EditMessage
// continues to work transparently when the routed token is a Telegram
// one.
func newSenderRouter(tg domain.Sender, larkWebhook *lark.Client, larkApp *lark.AppClient) *senderRouter {
	return &senderRouter{tg: tg, larkWebhook: larkWebhook, larkApp: larkApp}
}

// detectPlatform classifies a bot token. Exported only via the
// router's behaviour so tests can pin the contract without locking
// the symbol publicly. Order matters: the lark-app:// check runs
// before the https:// check so an https-shaped query value embedded
// in the lark-app URL cannot accidentally trip the webhook branch
// (theoretical, but cheap to be defensive).
func detectPlatform(botToken string) string {
	if strings.HasPrefix(botToken, lark.LarkAppTokenPrefix) {
		return platformLarkApp
	}
	if strings.HasPrefix(botToken, larkURLPrefix) {
		return domain.PlatformLark
	}
	return domain.PlatformTelegram
}

// Send routes by token prefix. Lark tokens go to the appropriate Lark
// client (chat and parseMode arguments are accepted but ignored for
// webhook bots — the chat is bound to the webhook URL itself);
// everything else goes to the Telegram adapter.
func (r *senderRouter) Send(ctx context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	switch detectPlatform(botToken) {
	case platformLarkApp:
		return r.larkApp.Send(ctx, botToken, chatID, parseMode, text)
	case domain.PlatformLark:
		return r.larkWebhook.Send(ctx, botToken, chatID, parseMode, text)
	default:
		return r.tg.Send(ctx, botToken, chatID, parseMode, text)
	}
}

// SendWithOpts is the buttons-aware path. Telegram messages keep
// their inline_keyboard support via the underlying SenderWithOpts
// adapter (the router type-asserts at dispatch time). For Lark the
// buttons are silently dropped — neither the custom-bot webhook nor
// the application IM API expose an inline_keyboard analogue — so we
// route to the plain Send. The worker is already prepared for
// "buttons silently dropped" in tests where the injected sender lacks
// SenderWithOpts; the same degradation applies here, intentionally,
// for Lark.
func (r *senderRouter) SendWithOpts(ctx context.Context, botToken, chatID, parseMode, text string, opts domain.SendOptions) (int64, error) {
	switch detectPlatform(botToken) {
	case platformLarkApp:
		// Lark IM API has no inline_keyboard. Drop buttons and forward.
		return r.larkApp.Send(ctx, botToken, chatID, parseMode, text)
	case domain.PlatformLark:
		return r.larkWebhook.Send(ctx, botToken, chatID, parseMode, text)
	}
	if sw, ok := r.tg.(domain.SenderWithOpts); ok {
		return sw.SendWithOpts(ctx, botToken, chatID, parseMode, text, opts)
	}
	return r.tg.Send(ctx, botToken, chatID, parseMode, text)
}

// EditMessage is the V7-2 state-machine collapse path. Telegram
// rewrites the existing message in place; Lark (both webhook and
// app modes) falls back to a fresh Send because neither client has
// a meaningful editMessageText analogue in the current wire-up.
// The lark.AppClient.Edit method exists for interface completeness
// but its messageID==0 fallback delegates to Send anyway, and
// message_threads stores int64s incompatible with Lark's opaque
// "om_..." message_id strings.
func (r *senderRouter) EditMessage(ctx context.Context, botToken, chatID string, messageID int64, parseMode, text string) error {
	switch detectPlatform(botToken) {
	case platformLarkApp:
		_, err := r.larkApp.Send(ctx, botToken, chatID, parseMode, text)
		return err
	case domain.PlatformLark:
		_, err := r.larkWebhook.Send(ctx, botToken, chatID, parseMode, text)
		return err
	}
	if sw, ok := r.tg.(domain.SenderWithOpts); ok {
		return sw.EditMessage(ctx, botToken, chatID, messageID, parseMode, text)
	}
	// Underlying tg sender lacks EditMessage (test fake). Fall back
	// to a fresh Send so the alert still lands; the worker's existing
	// fallback log path picks up the dropped collapse.
	_, err := r.tg.Send(ctx, botToken, chatID, parseMode, text)
	return err
}

// Compile-time conformance — when these break, an interface drift in
// internal/domain caught the wire-up regression here rather than in
// production traffic.
var (
	_ domain.Sender         = (*senderRouter)(nil)
	_ domain.SenderWithOpts = (*senderRouter)(nil)
)
