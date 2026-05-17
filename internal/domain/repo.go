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
	// CountByCreatorSince counts invites adminID created at or after
	// `since`. Used by the web layer to enforce the per-admin daily
	// generation cap so a compromised admin cannot mint unbounded
	// invites in a tight loop.
	CountByCreatorSince(ctx context.Context, adminID int64, since time.Time) (int, error)
}

// BotRepo manages tenant-owned bot rows. Implementations encrypt/decrypt
// BotToken transparently. ListAll is intentionally tenant-blind — only
// runtime wire-up (startup listener boot) and admin tooling should call
// it. All other surfaces MUST scope to a specific tenantID.
//
// SetEnabled is a narrow update used by the web layer (operator toggles
// via /enable|/disable) and by the runtime's 401 auto-disable callback.
// It is intentionally separate from Update so callers can flip the flag
// without re-encrypting the token or touching unrelated columns.
type BotRepo interface {
	Insert(ctx context.Context, b *Bot) error
	Update(ctx context.Context, b *Bot) error
	Delete(ctx context.Context, tenantID, id int64) error
	GetByID(ctx context.Context, tenantID, id int64) (*Bot, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*Bot, error)
	ListAll(ctx context.Context) ([]*Bot, error)
	// SetEnabled flips the enabled column on the (tenantID, id) row.
	// Returns ErrNotFound when no row matches (so the 401 auto-disable
	// path stays a no-op for a bot that has just been deleted).
	SetEnabled(ctx context.Context, tenantID, id int64, enabled bool) error
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

// SendOptions bundles optional per-call knobs the V7+ pipeline needs to
// pass to the underlying Telegram client without changing the legacy
// Sender contract. Buttons drives the inline_keyboard reply_markup
// (V7-1).
//
// Implementations that do not understand a field are expected to
// degrade to a vanilla sendMessage — callers must always tolerate
// "buttons silently dropped" because tests substitute legacy Senders.
type SendOptions struct {
	Buttons []PushButton
}

// SenderWithOpts extends Sender with the V7-1 button-aware send. A
// type assertion on the Sender dependency lets the worker keep the
// existing interface for tests that pre-date inline_keyboard while
// production wiring uses *tg.Client which satisfies both. EditMessage
// powers the V7-2 state-machine collapse: when a push carries a
// _fingerprint that already has a message_threads row the worker
// rewrites the existing Telegram message in-place instead of sending
// a new one.
type SenderWithOpts interface {
	Sender
	SendWithOpts(ctx context.Context, botToken, chatID, parseMode, text string, opts SendOptions) (msgID int64, err error)
	EditMessage(ctx context.Context, botToken, chatID string, messageID int64, parseMode, text string) error
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
	// ListByBot enumerates every ENABLED command owned by the same
	// tenant as botID. Powers the Telegram setMyCommands publisher and
	// the /commands built-in; both expose a public catalog, so disabled
	// rows MUST stay hidden. Same bot id convention as GetByBotAndName.
	ListByBot(ctx context.Context, botID int64) ([]*Command, error)
}

// SubscriberRepo records (command, bot, chat_id) tuples and exposes
// list/delete for the UI. Upsert is idempotent: re-inserting the same
// (command, chat, platform) triple bumps last_seen_at.
type SubscriberRepo interface {
	Upsert(ctx context.Context, s *Subscriber) error
	ListByCommand(ctx context.Context, tenantID, commandID int64) ([]*Subscriber, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*Subscriber, error)
	Delete(ctx context.Context, tenantID, id int64) error
	// DeleteByChatAndCommand removes the subscription row the listener
	// upserted when a chat first invoked a command. Powers the
	// /unsubscribe built-in; matches both "/name" and "name" shapes.
	// botID is the PulseGuard DB primary key (bots.id), same as
	// CommandRepo.GetByBotAndName / ListByBot.
	DeleteByChatAndCommand(ctx context.Context, botID int64, chatID, commandName string) error
}

// AlertAckRepo records "operator acknowledged alert <fingerprint>"
// events written by the Telegram /ack built-in. The push pipeline
// will (in a later sprint) consult GetByFingerprint to skip an
// already-acked storm; for now V6-3 only ships the audit trail.
//
// Insert returns store.ErrAlreadyAcked on (tenant, fingerprint)
// UNIQUE collisions so callers can render a friendly reply without
// surfacing SQL details.
type AlertAckRepo interface {
	Insert(ctx context.Context, a *AlertAck) error
	GetByFingerprint(ctx context.Context, tenantID int64, fingerprint string) (*AlertAck, error)
	ListByTenant(ctx context.Context, tenantID int64) ([]*AlertAck, error)
}

// MessageThreadRepo persists the (channel, fingerprint) → live
// Telegram message_id projection the V7-2 worker uses to collapse
// repeat alerts via editMessageText instead of sending duplicates.
//
// Upsert is idempotent: when the (channel_id, fingerprint) row
// already exists it stamps the row's tg_message_id + updated_at;
// otherwise it inserts a fresh row. GetByFingerprint returns
// ErrNotFound when nothing matches so the worker can branch on the
// classical Go error idiom without inspecting nil pointers.
type MessageThreadRepo interface {
	Upsert(ctx context.Context, m *MessageThread) error
	GetByFingerprint(ctx context.Context, channelID int64, fingerprint string) (*MessageThread, error)
	DeleteByChannel(ctx context.Context, tenantID, channelID int64) error
}

// SilenceRepo manages tenant-scoped silence rules driven by the V7-3
// Telegram /silence built-in. Match is the hot-path call: the worker
// invokes it per push before any Sender activity to decide whether to
// suppress the alert.
//
// Match's semantics: returns true when ANY active (now <= expires_at)
// silence row for the tenant has a Pattern that is a non-empty prefix
// of the supplied fingerprint. Empty patterns are ignored at insert
// time so they cannot create a "silence everything forever" footgun
// without explicit operator intent — the implementation rejects
// empty/whitespace patterns at Insert.
type SilenceRepo interface {
	Insert(ctx context.Context, s *Silence) error
	ListActive(ctx context.Context, tenantID int64, now time.Time) ([]*Silence, error)
	Delete(ctx context.Context, tenantID, id int64) error
	// DeleteByPattern removes every active silence row whose pattern
	// matches the supplied string exactly. Returns the number of
	// affected rows so /unsilence can craft a helpful reply.
	DeleteByPattern(ctx context.Context, tenantID int64, pattern string) (int64, error)
	Match(ctx context.Context, tenantID int64, fingerprint string, now time.Time) (bool, error)
}
