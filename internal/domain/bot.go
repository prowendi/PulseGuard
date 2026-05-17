package domain

import "time"

// Platform identifiers used by the bot listener manager.
//
//   - PlatformTelegram: full bidirectional support (push + long-poll
//     listener + inline keyboards + editMessageText).
//   - PlatformLark: push-only via Lark Custom Bot Webhook OR
//     bidirectional via Lark App Bot + events subscription.
//   - PlatformSMTP: push-only via SMTP relay. No listener; no
//     commands. Channel.chat_id is repurposed as a comma-separated
//     recipient list. Subject is derived from the first non-empty
//     line of the rendered template body, with "\n\n" separating
//     subject from body.
//
// The column exists in the bots table so adding more platforms
// (Discord, Slack, WeChat, ...) stays a code-only change.
const (
	PlatformTelegram = "telegram"
	PlatformLark     = "lark"
	PlatformSMTP     = "smtp"
)

// IsValidPlatform reports whether p is a platform PulseGuard knows how
// to deliver pushes for. Keep this list as the single source of truth
// so repos, API validation, and the platform manager agree.
//
// NOTE: a platform being valid here does NOT imply it has a Listener
// (inbound long-poll) — Lark Webhook + SMTP, for example, are
// push-only. The listener Manager separately silently-skips platforms
// it has no Factory for.
func IsValidPlatform(p string) bool {
	switch p {
	case PlatformTelegram, PlatformLark, PlatformSMTP:
		return true
	default:
		return false
	}
}

// BotKind partitions the lark-platform rows into the two flavours
// PulseGuard supports. "webhook" is the Phase A single-direction
// Custom Bot Webhook (the BotToken field carries the full
// https://open.feishu.cn/open-apis/bot/v2/hook/<key> URL). "app" is
// the Phase B Lark application bot: OAuth2 tenant_access_token + IM
// API send + event subscription, with credentials stored in the
// dedicated AppID / AppSecret / VerifyToken / EncryptKey columns
// (migration 0010).
//
// Telegram bots always carry BotKindWebhook — the field is meaningful
// only on Platform=="lark" rows. The default in the schema is
// "webhook" so every pre-Phase-B row keeps its behaviour.
const (
	BotKindWebhook = "webhook"
	BotKindApp     = "app"
)

// IsValidBotKind reports whether k is a kind the runtime knows how to
// route. SQLite enforces the same allow-list on inserts via the
// migration 0010 CHECK constraint, but the application layer also
// checks so callers receive ErrValidation instead of a raw SQL
// constraint failure.
func IsValidBotKind(k string) bool {
	switch k {
	case BotKindWebhook, BotKindApp:
		return true
	default:
		return false
	}
}

// Bot is a tenant-owned remote-platform identity used to deliver pushes.
// Platform names the chat platform this bot speaks ("telegram" or
// "lark"). BotKind further partitions lark rows into "webhook" vs
// "app". BotToken is decrypted on read from the repo and must never be
// logged or serialised verbatim.
//
// Lark-app credentials live in AppID / AppSecret / VerifyToken /
// EncryptKey (migration 0010). AppSecret is AES-GCM encrypted at rest
// using the same master_key_b64 cipher that protects BotToken; the
// other three fields are plaintext (AppID is public, VerifyToken is
// only a callback identifier, EncryptKey is the HMAC key used to
// verify inbound event signatures — see internal/web/lark_events_api.go).
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
	Platform    string // "telegram" | "lark"
	BotKind     string // "webhook" | "app" (meaningful only when Platform=="lark")
	BotToken    string // plaintext (set after store-layer decryption); empty for app-kind lark rows
	Description string
	Enabled     bool

	// AppID / AppSecret / VerifyToken / EncryptKey are populated only
	// for (Platform=="lark", BotKind=="app") rows. AppSecret is
	// returned in plaintext after store-layer decryption and must
	// never be logged or serialised back to clients.
	AppID        string
	AppSecret    string
	VerifyToken  string
	EncryptKey   string

	// SMTPHost / Port / Username / Password / From / UseTLS are
	// populated only for Platform=="smtp" rows. SMTPPassword is the
	// plaintext credential — encrypted at rest via the AES-GCM
	// cipher and decrypted on read by the bot repo. From, when
	// empty, falls back to SMTPUsername during send.
	SMTPHost     string
	SMTPPort     int
	SMTPUsername string
	SMTPPassword string
	SMTPFrom     string
	SMTPUseTLS   bool

	CreatedAt time.Time
	UpdatedAt time.Time
}
