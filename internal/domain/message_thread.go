package domain

import "time"

// MessageThread maps a (channel, fingerprint) pair to the live
// Telegram message id PulseGuard has already sent for that logical
// alert. The V7-2 push pipeline consults this table before sending:
// when a row exists for the inbound (channel, fingerprint) it edits
// the existing Telegram message in place (collapsing storms into a
// single, updating chat entry) rather than queuing yet another
// notification.
//
// One row per (channel_id, fingerprint); the UNIQUE constraint in
// migration 0008 enforces it so Upsert is race-safe.
type MessageThread struct {
	ID          int64
	ChannelID   int64
	TenantID    int64
	Fingerprint string
	ChatID      string
	TGMessageID int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
