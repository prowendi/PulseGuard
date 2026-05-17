// Package runtime — sender_router multiplexes the worker's single
// domain.Sender dependency across multiple chat-platform clients
// (Telegram + Lark / 飞书 today; pluggable for Discord / Slack / WeChat
// once their outbound clients land).
//
// Detection happens on the BotToken value the worker passes through
// from domain.Bot, NOT on bot.Platform. The router does not see the
// Bot row — it sees only the wire-level credentials the worker hands
// to Sender.Send. The chosen heuristic is a prefix test:
//
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

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/lark"
)

// larkURLPrefix is the canonical custom-bot webhook host. detectPlatform
// compares case-sensitively because URL hosts are not case-insensitive
// in the routing layer — operators who paste a mixed-case host get
// telegram routing (which then errors at the tg layer with a clean
// "bot token is not valid" instead of mis-routing to Lark).
const larkURLPrefix = "https://open.feishu.cn/"

// senderRouter is the multi-platform dispatcher. tg is the existing
// telegram adapter (SenderWithOpts capable); larkClient is the new
// Lark client. Both fields are required at construction time — there
// is no nil-fallback because a bot row carrying the "wrong" platform
// for a nil client should fail loudly rather than silently send
// nowhere.
type senderRouter struct {
	tg   domain.Sender
	lark *lark.Client
}

// newSenderRouter wires the two underlying clients. Pass the existing
// *tgSenderAdapter (or any domain.Sender for tests) as tg, and a
// *lark.Client as larkClient. The returned value satisfies BOTH
// domain.Sender AND domain.SenderWithOpts, so the worker's type
// assertion for SendWithOpts / EditMessage continues to work
// transparently when the routed token is a Telegram one.
func newSenderRouter(tg domain.Sender, larkClient *lark.Client) *senderRouter {
	return &senderRouter{tg: tg, lark: larkClient}
}

// detectPlatform classifies a bot token. Exported only via the
// router's behaviour so tests can pin the contract without locking
// the symbol publicly.
func detectPlatform(botToken string) string {
	if strings.HasPrefix(botToken, larkURLPrefix) {
		return domain.PlatformLark
	}
	return domain.PlatformTelegram
}

// Send routes by token prefix. Lark tokens go to the lark.Client (chat
// and parseMode arguments are ignored by Lark but forwarded for shape
// uniformity); everything else goes to the Telegram adapter.
func (r *senderRouter) Send(ctx context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	if detectPlatform(botToken) == domain.PlatformLark {
		return r.lark.Send(ctx, botToken, chatID, parseMode, text)
	}
	return r.tg.Send(ctx, botToken, chatID, parseMode, text)
}

// SendWithOpts is the buttons-aware path. Telegram messages keep
// their inline_keyboard support via the underlying SenderWithOpts
// adapter (the router type-asserts at dispatch time). For Lark the
// buttons are silently dropped — custom-bot webhooks have no
// inline_keyboard analogue — so we route to the plain Send. The
// worker is already prepared for "buttons silently dropped" in tests
// where the injected sender lacks SenderWithOpts; the same
// degradation applies here, intentionally, for Lark.
func (r *senderRouter) SendWithOpts(ctx context.Context, botToken, chatID, parseMode, text string, opts domain.SendOptions) (int64, error) {
	if detectPlatform(botToken) == domain.PlatformLark {
		// Lark has no inline_keyboard. Drop buttons and forward.
		return r.lark.Send(ctx, botToken, chatID, parseMode, text)
	}
	if sw, ok := r.tg.(domain.SenderWithOpts); ok {
		return sw.SendWithOpts(ctx, botToken, chatID, parseMode, text, opts)
	}
	return r.tg.Send(ctx, botToken, chatID, parseMode, text)
}

// EditMessage is the V7-2 state-machine collapse path. Telegram
// rewrites the existing message in place; Lark falls back to a fresh
// Send (lark.Client.Edit already implements that fallback, but we go
// through Send here to keep the router's call graph flat and the
// degradation explicit).
func (r *senderRouter) EditMessage(ctx context.Context, botToken, chatID string, messageID int64, parseMode, text string) error {
	if detectPlatform(botToken) == domain.PlatformLark {
		_, err := r.lark.Send(ctx, botToken, chatID, parseMode, text)
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
