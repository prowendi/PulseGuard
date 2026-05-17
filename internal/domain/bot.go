package domain

import "time"

// Platform identifiers used by the bot listener manager. Only "telegram"
// is supported in the current MVP; the column exists so a follow-up can
// plug Discord/Slack/WeChat listeners in without a schema change.
const (
	PlatformTelegram = "telegram"
)

// IsValidPlatform reports whether p is a platform PulseGuard knows how to
// run a listener for. Keep this list as the single source of truth so
// repos, API validation, and the platform manager agree.
func IsValidPlatform(p string) bool {
	return p == PlatformTelegram
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
