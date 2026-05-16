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
type Bot struct {
	ID          int64
	TenantID    int64
	Name        string
	Platform    string // currently always "telegram"
	BotToken    string // plaintext (set after store-layer decryption)
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
