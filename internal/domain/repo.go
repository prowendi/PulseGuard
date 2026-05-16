package domain

import (
	"context"
	"time"
)

// TenantRepo manages tenant rows.
type TenantRepo interface {
	Insert(ctx context.Context, t *Tenant) error
	GetByEmail(ctx context.Context, email string) (*Tenant, error)
	GetByID(ctx context.Context, id int64) (*Tenant, error)
	CountActive(ctx context.Context) (int, error)
}

// InviteRepo manages invite codes. Lock acquires SELECT ... FOR UPDATE
// semantics within a transaction so Consume cannot race.
type InviteRepo interface {
	Insert(ctx context.Context, code *InviteCode) error
	Lock(ctx context.Context, code string) (*InviteCode, error)
	Consume(ctx context.Context, code string, tenantID int64) error
	ListByCreator(ctx context.Context, adminID int64) ([]*InviteCode, error)
}

// BotRepo manages tenant-owned bot rows. Implementations encrypt/decrypt
// BotToken transparently.
type BotRepo interface {
	Insert(ctx context.Context, b *Bot) error
	Update(ctx context.Context, b *Bot) error
	Delete(ctx context.Context, tenantID, id int64) error
	GetByID(ctx context.Context, tenantID, id int64) (*Bot, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*Bot, error)
}

// TemplateRepo manages tenant message templates.
type TemplateRepo interface {
	Insert(ctx context.Context, t *Template) error
	Update(ctx context.Context, t *Template) error
	Delete(ctx context.Context, tenantID, id int64) error
	GetByID(ctx context.Context, tenantID, id int64) (*Template, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*Template, error)
}

// ChannelRepo manages tenant channels keyed by push_token.
type ChannelRepo interface {
	Insert(ctx context.Context, c *Channel) error
	Update(ctx context.Context, c *Channel) error
	Delete(ctx context.Context, tenantID, id int64) error
	GetByID(ctx context.Context, tenantID, id int64) (*Channel, error)
	GetByPushToken(ctx context.Context, pushToken string) (*Channel, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*Channel, error)
}

// OutboxRepo is the heart of the push pipeline. ClaimNext implements
// row-level lease semantics via UPDATE ... RETURNING.
type OutboxRepo interface {
	Insert(ctx context.Context, item *PushOutbox) (int64, error)
	ClaimNext(ctx context.Context, workerID string, now time.Time) (*PushOutbox, error)
	MarkSent(ctx context.Context, id int64, now time.Time) error
	MarkRetry(ctx context.Context, id int64, nextAt time.Time, reason string) error
	MarkDead(ctx context.Context, id int64, reason string) error
	ReclaimInFlight(ctx context.Context, olderThan time.Time) (int64, error)
}

// LogRepo records the terminal outcome of every push attempt.
type LogRepo interface {
	Insert(ctx context.Context, log *PushLog) error
	ListByTenant(ctx context.Context, tenantID int64, page, perPage int) ([]*PushLog, int, error)
	ListByChannel(ctx context.Context, tenantID, channelID int64, page, perPage int) ([]*PushLog, int, error)
	PurgeOlderThan(ctx context.Context, t time.Time) (int64, error)
}

// DedupRepo implements the per-channel fingerprint window.
type DedupRepo interface {
	SeenOrInsert(ctx context.Context, channelID int64, fp string, now time.Time, windowSec int) (alreadySeen bool, err error)
	PurgeExpired(ctx context.Context, now time.Time) (int64, error)
}

// RateLimiter is a per-channel token bucket persisted in SQLite.
type RateLimiter interface {
	Allow(ctx context.Context, channelID int64, ratePerMin int) (bool, error)
}

// DeadLetterRepo stores terminally-failed pushes for inspection and replay.
type DeadLetterRepo interface {
	Insert(ctx context.Context, dl *DeadLetter) error
	ListByTenant(ctx context.Context, tenantID int64, page, perPage int) ([]*DeadLetter, int, error)
	Replay(ctx context.Context, tenantID, id int64) (newOutboxID int64, err error)
}

// SessionRepo manages auth sessions.
type SessionRepo interface {
	Insert(ctx context.Context, s *Session) error
	GetByID(ctx context.Context, id string) (*Session, error)
	Delete(ctx context.Context, id string) error
	PurgeExpired(ctx context.Context, now time.Time) (int64, error)
}

// Sender is the outbound Telegram bot client interface. Implementations
// classify errors via the tg package.
type Sender interface {
	Send(ctx context.Context, botToken, chatID, parseMode, text string) (msgID int64, err error)
}
