package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// MessageThreadRepo persists the (channel, fingerprint) → live
// Telegram message_id projection consumed by the V7-2 push worker to
// collapse repeat alerts via editMessageText. The repo is
// intentionally tiny: it never reads logs, never makes a routing
// decision — it only owns CRUD against message_threads.
type MessageThreadRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewMessageThreadRepo binds the repo to a DB handle and clock.
func NewMessageThreadRepo(db *sql.DB, clock domain.Clock) *MessageThreadRepo {
	return &MessageThreadRepo{db: db, clock: clock}
}

// Upsert writes the (channel_id, fingerprint) row, replacing any
// existing tg_message_id + chat_id + updated_at. Implementation note:
// SQLite supports ON CONFLICT … DO UPDATE since 3.24; the unique
// index from migration 0008 ensures only one row per logical alert
// so the conflict target is unambiguous.
//
// Returns nil on success; SQL-level errors propagate verbatim so
// callers can decide between retry and DLQ.
func (r *MessageThreadRepo) Upsert(ctx context.Context, m *domain.MessageThread) error {
	if m == nil {
		return errors.New("message_thread: nil")
	}
	if m.ChannelID == 0 {
		return fmt.Errorf("%w: channel_id is zero", domain.ErrValidation)
	}
	if m.TenantID == 0 {
		return fmt.Errorf("%w: tenant_id is zero", domain.ErrValidation)
	}
	m.Fingerprint = strings.TrimSpace(m.Fingerprint)
	if m.Fingerprint == "" {
		return fmt.Errorf("%w: fingerprint is empty", domain.ErrValidation)
	}
	if strings.TrimSpace(m.ChatID) == "" {
		return fmt.Errorf("%w: chat_id is empty", domain.ErrValidation)
	}
	if m.TGMessageID == 0 {
		return fmt.Errorf("%w: tg_message_id is zero", domain.ErrValidation)
	}
	now := nowMs(r.clock)
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO message_threads
		  (channel_id, tenant_id, fingerprint, chat_id, tg_message_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(channel_id, fingerprint) DO UPDATE SET
		  chat_id       = excluded.chat_id,
		  tg_message_id = excluded.tg_message_id,
		  updated_at    = excluded.updated_at`,
		m.ChannelID, m.TenantID, m.Fingerprint, m.ChatID, m.TGMessageID, now, now)
	if err != nil {
		return fmt.Errorf("upsert message_thread: %w", err)
	}
	id, err := res.LastInsertId()
	if err == nil && id != 0 {
		m.ID = id
	}
	m.UpdatedAt = toTime(now)
	if m.CreatedAt.IsZero() {
		m.CreatedAt = toTime(now)
	}
	return nil
}

// GetByFingerprint returns the live thread row for a
// (channel_id, fingerprint) pair. Returns domain.ErrNotFound when no
// row matches so the worker can branch on a typed sentinel without
// inspecting nils.
func (r *MessageThreadRepo) GetByFingerprint(ctx context.Context, channelID int64, fingerprint string) (*domain.MessageThread, error) {
	fp := strings.TrimSpace(fingerprint)
	if channelID == 0 || fp == "" {
		return nil, domain.ErrNotFound
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, channel_id, tenant_id, fingerprint, chat_id, tg_message_id, created_at, updated_at
		  FROM message_threads
		 WHERE channel_id = ? AND fingerprint = ?`, channelID, fp)
	m := &domain.MessageThread{}
	var createdMs, updatedMs int64
	err := row.Scan(&m.ID, &m.ChannelID, &m.TenantID, &m.Fingerprint,
		&m.ChatID, &m.TGMessageID, &createdMs, &updatedMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan message_thread: %w", err)
	}
	m.CreatedAt = toTime(createdMs)
	m.UpdatedAt = toTime(updatedMs)
	return m, nil
}

// DeleteByChannel removes every thread row attached to the supplied
// channel. The CASCADE on the channels FK already deletes rows when
// the channel itself is dropped; this method exists so an operator
// can wipe just the in-flight thread state (e.g. after a Telegram
// chat migration) without nuking the channel record.
func (r *MessageThreadRepo) DeleteByChannel(ctx context.Context, tenantID, channelID int64) error {
	if tenantID == 0 || channelID == 0 {
		return fmt.Errorf("%w: tenant_id and channel_id required", domain.ErrValidation)
	}
	_, err := r.db.ExecContext(ctx,
		`DELETE FROM message_threads WHERE channel_id = ? AND tenant_id = ?`, channelID, tenantID)
	if err != nil {
		return fmt.Errorf("delete message_threads: %w", err)
	}
	return nil
}

// Ensure interface compliance at compile time.
var _ domain.MessageThreadRepo = (*MessageThreadRepo)(nil)
