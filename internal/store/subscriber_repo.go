package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
)

// SubscriberRepo records (command_id, bot_id, chat_id, platform)
// tuples that have invoked a custom command. Upsert is idempotent.
type SubscriberRepo struct {
	db    *sql.DB
	clock domain.Clock
}

// NewSubscriberRepo binds the repo to a DB handle and clock.
func NewSubscriberRepo(db *sql.DB, clock domain.Clock) *SubscriberRepo {
	return &SubscriberRepo{db: db, clock: clock}
}

// Upsert inserts a subscriber row, or bumps last_seen_at when the
// (command_id, chat_id, platform) triple already exists. SQLite's
// "ON CONFLICT ... DO UPDATE" is used so the operation is atomic and
// the returned domain.Subscriber.ID is populated in both code paths.
func (r *SubscriberRepo) Upsert(ctx context.Context, s *domain.Subscriber) error {
	if err := validateSubscriber(s); err != nil {
		return err
	}
	now := nowMs(r.clock)
	if s.Platform == "" {
		s.Platform = domain.PlatformTelegram
	}

	// Atomic upsert: INSERT bumps last_seen_at on conflict, then a
	// follow-up SELECT pulls the row so we can return both the id
	// (which may be the existing one) and the canonical timestamps.
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO subscribers (tenant_id, command_id, bot_id, chat_id, platform, created_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(command_id, chat_id, platform) DO UPDATE
		   SET last_seen_at = excluded.last_seen_at,
		       bot_id       = excluded.bot_id`,
		s.TenantID, s.CommandID, s.BotID, s.ChatID, s.Platform, now, now)
	if err != nil {
		return fmt.Errorf("upsert subscriber: %w", err)
	}
	row := r.db.QueryRowContext(ctx, `
		SELECT id, tenant_id, command_id, bot_id, chat_id, platform, created_at, last_seen_at
		  FROM subscribers
		 WHERE command_id = ? AND chat_id = ? AND platform = ?`,
		s.CommandID, s.ChatID, s.Platform)
	scanned, err := scanSubscriber(row)
	if err != nil {
		return fmt.Errorf("read upserted subscriber: %w", err)
	}
	*s = *scanned
	return nil
}

// ListByCommand returns every subscriber row of the given command,
// scoped to tenant for safety.
func (r *SubscriberRepo) ListByCommand(ctx context.Context, tenantID, commandID int64) ([]*domain.Subscriber, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, command_id, bot_id, chat_id, platform, created_at, last_seen_at
		  FROM subscribers
		 WHERE tenant_id = ? AND command_id = ?
		 ORDER BY id ASC`, tenantID, commandID)
	if err != nil {
		return nil, fmt.Errorf("list subscribers by command: %w", err)
	}
	defer rows.Close()
	return collectSubscribers(rows)
}

// ListByTenant returns every subscriber row of a tenant.
func (r *SubscriberRepo) ListByTenant(ctx context.Context, tenantID int64) ([]*domain.Subscriber, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, tenant_id, command_id, bot_id, chat_id, platform, created_at, last_seen_at
		  FROM subscribers
		 WHERE tenant_id = ?
		 ORDER BY id ASC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("list subscribers by tenant: %w", err)
	}
	defer rows.Close()
	return collectSubscribers(rows)
}

// Delete removes a subscriber row owned by tenantID.
func (r *SubscriberRepo) Delete(ctx context.Context, tenantID, id int64) error {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM subscribers WHERE id = ? AND tenant_id = ?`, id, tenantID)
	if err != nil {
		return fmt.Errorf("delete subscriber: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// DeleteByChatAndCommand removes the subscription a chat has against
// a (bot, command-name) tuple — the row Upsert creates the first time
// a chat invokes a command. Powers the listener's built-in
// /unsubscribe so users can opt out without involving the operator.
//
// The command name is matched against the commands.name column AND
// also against "/"+name because operators define commands in either
// shape; both join the same row by (tenant via bots) so the matching
// is symmetric. Returns ErrNotFound when no row matches (the listener
// turns that into a friendly "未订阅" reply).
//
// botID is the PulseGuard DB primary key (bots.id), same convention
// as CommandRepo.GetByBotAndName / ListByBot.
func (r *SubscriberRepo) DeleteByChatAndCommand(ctx context.Context, botID int64, chatID, commandName string) error {
	commandName = strings.TrimSpace(commandName)
	if commandName == "" {
		return fmt.Errorf("%w: command name empty", domain.ErrValidation)
	}
	if strings.TrimSpace(chatID) == "" {
		return fmt.Errorf("%w: chat id empty", domain.ErrValidation)
	}
	slashName := "/" + strings.TrimPrefix(commandName, "/")
	bareName := strings.TrimPrefix(commandName, "/")
	res, err := r.db.ExecContext(ctx, `
		DELETE FROM subscribers
		 WHERE bot_id = ?
		   AND chat_id = ?
		   AND command_id IN (
		     SELECT c.id FROM commands c
		      JOIN bots b ON b.tenant_id = c.tenant_id
		      WHERE b.id = ?
		        AND (c.name = ? OR c.name = ?)
		   )`, botID, chatID, botID, slashName, bareName)
	if err != nil {
		return fmt.Errorf("delete subscriber by chat+command: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func collectSubscribers(rows *sql.Rows) ([]*domain.Subscriber, error) {
	var out []*domain.Subscriber
	for rows.Next() {
		s, err := scanSubscriber(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func scanSubscriber(s interface {
	Scan(dest ...any) error
}) (*domain.Subscriber, error) {
	sub := &domain.Subscriber{}
	var createdMs, lastSeenMs int64
	err := s.Scan(&sub.ID, &sub.TenantID, &sub.CommandID, &sub.BotID,
		&sub.ChatID, &sub.Platform, &createdMs, &lastSeenMs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan subscriber: %w", err)
	}
	sub.CreatedAt = toTime(createdMs)
	sub.LastSeenAt = toTime(lastSeenMs)
	return sub, nil
}

func validateSubscriber(s *domain.Subscriber) error {
	if s == nil {
		return errors.New("subscriber is nil")
	}
	if s.TenantID == 0 {
		return fmt.Errorf("%w: subscriber tenant_id is zero", domain.ErrValidation)
	}
	if s.CommandID == 0 {
		return fmt.Errorf("%w: subscriber command_id is zero", domain.ErrValidation)
	}
	if s.BotID == 0 {
		return fmt.Errorf("%w: subscriber bot_id is zero", domain.ErrValidation)
	}
	if strings.TrimSpace(s.ChatID) == "" {
		return fmt.Errorf("%w: subscriber chat_id is empty", domain.ErrValidation)
	}
	return nil
}

// Ensure interface compliance at compile time.
var _ domain.SubscriberRepo = (*SubscriberRepo)(nil)
