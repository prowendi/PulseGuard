package domain

import "time"

// Platform identifiers used by the bot listener manager.
//
//   - PlatformTelegram: full bidirectional support (push + long-poll
//     listener + inline keyboards + editMessageText).
//   - PlatformLark: push-only via Lark Custom Bot Webhook. The
//     BotToken field stores the full webhook URL
//     (https://open.feishu.cn/open-apis/bot/v2/hook/<key>). No listener
//     is spawned (Lark custom bots are one-way); inline keyboards and
//     editMessageText are silently degraded by the worker.
//
// The column exists in the bots table so adding more platforms
// (Discord, Slack, WeChat, ...) stays a code-only change.
const (
	PlatformTelegram = "telegram"
	PlatformLark     = "lark"
)

// IsValidPlatform reports whether p is a platform PulseGuard knows how
// to deliver pushes for. Keep this list as the single source of truth
// so repos, API validation, and the platform manager agree.
//
// NOTE: a platform being valid here does NOT imply it has a Listener
// (inbound long-poll) — Lark, for example, is push-only. The listener
// Manager separately silently-skips platforms it has no Factory for.
func IsValidPlatform(p string) bool {
	switch p {
	case PlatformTelegram, PlatformLark:
		return true
	default:
		return false
	}
}

// Bot is a tenant-owned remote-platform identity used to deliver pushes.
// Platform names the chat platform this bot speaks (currently only
// "telegram"). BotToken is decrypted on read from the repo and must
// never be logged or serialised verbatim.
//
// Enabled mirrors the bots.enabled column (migration 0005). When false,
// the platform Manager refuses to spawn (or restart) the long-poll loop
// for this bot, and the runtime startup wire-up skips it. Outbound
// delivery via the worker pool currently does not branch on Enabled —
// pause is enforced at the listener layer; toggling delivery off is a
// channel-level concern. Operators flip this via the /api/v1/bots/{id}
// /enable|/disable endpoints, and the runtime auto-flips it to false
// when Telegram surfaces ErrTokenInvalid (401).
type Bot struct {
	ID          int64
	TenantID    int64
	Name        string
	Platform    string // currently always "telegram"
	BotToken    string // plaintext (set after store-layer decryption)
	Description string
	Enabled     bool
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
