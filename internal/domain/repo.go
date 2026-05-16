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
	// Delete removes an unused invite code owned by adminID. Returns
	// ErrNotFound if no row matches that (code, created_by) pair, and
	// ErrInviteInvalid if the code has already been consumed.
	Delete(ctx context.Context, code string, adminID int64) error
}

// BotRepo manages tenant-owned bot rows. Implementations encrypt/decrypt
// BotToken transparently. ListAll is intentionally tenant-blind — only
// runtime wire-up (startup listener boot) and admin tooling should call
// it. All other surfaces MUST scope to a specific tenantID.
type BotRepo interface {
	Insert(ctx context.Context, b *Bot) error
	Update(ctx context.Context, b *Bot) error
	Delete(ctx context.Context, tenantID, id int64) error
	GetByID(ctx context.Context, tenantID, id int64) (*Bot, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*Bot, error)
	ListAll(ctx context.Context) ([]*Bot, error)
}

// TemplateRepo manages tenant message templates.
type TemplateRepo interface {
	Insert(ctx context.Context, t *Template) error
	Update(ctx context.Context, t *Template) error
	Delete(ctx context.Context, tenantID, id int64) error
	GetByID(ctx context.Context, tenantID, id int64) (*Template, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*Template, error)
}

// ChannelRepo manages tenant channels keyed by push_token plus their
// many-to-many template bindings. Reads always hydrate Channel.Templates
// so callers can use DefaultTemplateID() / HasTemplate() without an
// extra round-trip.
type ChannelRepo interface {
	Insert(ctx context.Context, c *Channel) error
	Update(ctx context.Context, c *Channel) error
	Delete(ctx context.Context, tenantID, id int64) error
	GetByID(ctx context.Context, tenantID, id int64) (*Channel, error)
	GetByPushToken(ctx context.Context, pushToken string) (*Channel, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*Channel, error)
	// ReplaceTemplates atomically swaps the channel's template bindings
	// to the supplied list. Used by the UI handler when the user edits
	// only the "bound templates" form section. tenantID enforces
	// ownership; ErrNotFound is returned when channelID is not visible
	// to the tenant.
	ReplaceTemplates(ctx context.Context, tenantID, channelID int64, bindings []*ChannelTemplate) error
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

// CommandRepo manages tenant-owned Starlark commands. GetByBotAndName
// is the listener's hot-path resolver: it joins commands to bots via
// the bots.tenant_id column so a listener can dispatch with just the
// (bot_id, name) pair available in an inbound Telegram update.
type CommandRepo interface {
	Insert(ctx context.Context, c *Command) error
	Update(ctx context.Context, c *Command) error
	Delete(ctx context.Context, tenantID, id int64) error
	GetByID(ctx context.Context, tenantID, id int64) (*Command, error)
	GetByTenantAndName(ctx context.Context, tenantID int64, name string) (*Command, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*Command, error)
	// GetByBotAndName resolves the tenant-scoped command for the bot
	// owner. ErrNotFound is returned for unknown bot, unknown command,
	// or cross-tenant mismatches. Listener-only API.
	GetByBotAndName(ctx context.Context, botID int64, name string) (*Command, error)
}

// SubscriberRepo records (command, bot, chat_id) tuples and exposes
// list/delete for the UI. Upsert is idempotent: re-inserting the same
// (command, chat, platform) triple bumps last_seen_at.
type SubscriberRepo interface {
	Upsert(ctx context.Context, s *Subscriber) error
	ListByCommand(ctx context.Context, tenantID, commandID int64) ([]*Subscriber, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*Subscriber, error)
	Delete(ctx context.Context, tenantID, id int64) error
}
