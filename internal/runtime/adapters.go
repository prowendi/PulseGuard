package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/platform/telegram"
	"github.com/wendi/pulseguard/internal/store"
)

// commandCatalogAdapter projects domain.CommandRepo into the narrow
// telegram.CommandCatalog interface the listener consumes. Keeping the
// projection here (not in the telegram package) means the listener
// never imports domain types beyond what it owns, and the runtime
// retains the freedom to swap repo implementations without rippling
// changes into the platform adapter.
type commandCatalogAdapter struct {
	commands domain.CommandRepo
}

// ListByBot implements telegram.CommandCatalog. Disabled commands are
// already filtered out by the repo (the listener treats disabled =
// hidden), so this adapter is a pure 1:1 projection.
func (a commandCatalogAdapter) ListByBot(ctx context.Context, botID int64) ([]telegram.CommandSummary, error) {
	rows, err := a.commands.ListByBot(ctx, botID)
	if err != nil {
		return nil, err
	}
	out := make([]telegram.CommandSummary, 0, len(rows))
	for _, c := range rows {
		if c == nil {
			continue
		}
		out = append(out, telegram.CommandSummary{
			Name:        c.Name,
			Description: c.Description,
		})
	}
	return out, nil
}

// subscriberRemoverAdapter projects domain.SubscriberRepo into the
// narrow telegram.SubscriberRemover interface the listener consumes.
// Same separation-of-concerns rationale as commandCatalogAdapter.
type subscriberRemoverAdapter struct {
	subscribers domain.SubscriberRepo
}

// DeleteByChatAndCommand implements telegram.SubscriberRemover by
// forwarding to the underlying repo. The repo enforces tenant
// scoping via the bots JOIN, so this layer is a pure pass-through.
func (a subscriberRemoverAdapter) DeleteByChatAndCommand(ctx context.Context, botID int64, chatID, commandName string) error {
	return a.subscribers.DeleteByChatAndCommand(ctx, botID, chatID, commandName)
}

// alertAckerAdapter projects domain.AlertAckRepo into the narrow
// telegram.AlertAcker interface the listener consumes for the /ack
// built-in. The bot row is read once per invocation to resolve the
// tenant_id the audit row needs — listener-supplied botID is the
// PulseGuard DB primary key so the lookup never crosses tenants.
//
// store.ErrAlreadyAcked is translated to telegram.ErrAckAlreadyExists
// so the listener stays decoupled from the store package's error
// sentinels.
type alertAckerAdapter struct {
	acks domain.AlertAckRepo
	bots domain.BotRepo
}

// Insert resolves tenant_id from the bot row, then writes the ack.
// Tenant resolution uses ListAll because BotRepo.GetByID needs a
// tenant scope by contract — a small lookup table here is the price
// of keeping the BotRepo interface symmetric. In practice the bots
// list is short (single-digit per tenant) and the call is rare, so
// the linear scan is fine.
func (a alertAckerAdapter) Insert(ctx context.Context, in telegram.AckInput) error {
	tenantID, err := a.tenantForBot(ctx, in.BotID)
	if err != nil {
		return err
	}
	err = a.acks.Insert(ctx, &domain.AlertAck{
		TenantID:    tenantID,
		Fingerprint: in.Fingerprint,
		AckedBy:     in.AckedBy,
		BotID:       in.BotID,
		ChatID:      in.ChatID,
	})
	if errors.Is(err, store.ErrAlreadyAcked) {
		return telegram.ErrAckAlreadyExists
	}
	return err
}

func (a alertAckerAdapter) tenantForBot(ctx context.Context, botID int64) (int64, error) {
	bots, err := a.bots.ListAll(ctx)
	if err != nil {
		return 0, err
	}
	for _, b := range bots {
		if b != nil && b.ID == botID {
			return b.TenantID, nil
		}
	}
	return 0, domain.ErrNotFound
}

// Compile-time conformance.
var (
	_ telegram.CommandCatalog    = commandCatalogAdapter{}
	_ telegram.SubscriberRemover = subscriberRemoverAdapter{}
	_ telegram.AlertAcker        = alertAckerAdapter{}
	_ telegram.SilenceManager    = silenceManagerAdapter{}
)

// silenceManagerAdapter projects domain.SilenceRepo + BotRepo into the
// telegram.SilenceManager interface. The listener supplies a botID; the
// adapter resolves the tenant via the bots table before forwarding to
// the underlying silence repo. The clock is supplied by runtime so
// ExpiresAt stamping is deterministic in tests.
type silenceManagerAdapter struct {
	silences domain.SilenceRepo
	bots     domain.BotRepo
	clock    domain.Clock
}

// Insert parses the listener's (pattern, duration) into a Silence and
// hands it to the underlying repo. Duration is converted to an absolute
// ExpiresAt by adding to the injected clock so a FakeClock test gets a
// deterministic expiry.
func (a silenceManagerAdapter) Insert(ctx context.Context, in telegram.SilenceInsertInput) error {
	tenantID, err := a.tenantForBot(ctx, in.BotID)
	if err != nil {
		return err
	}
	now := a.clock.Now()
	return a.silences.Insert(ctx, &domain.Silence{
		TenantID:  tenantID,
		Pattern:   in.Pattern,
		CreatedBy: in.CreatedBy,
		ExpiresAt: now.Add(in.Duration),
	})
}

// List returns active silences for the bot's tenant, projected into
// the listener-facing SilenceSummary shape.
func (a silenceManagerAdapter) List(ctx context.Context, botID int64) ([]telegram.SilenceSummary, error) {
	tenantID, err := a.tenantForBot(ctx, botID)
	if err != nil {
		return nil, err
	}
	rows, err := a.silences.ListActive(ctx, tenantID, a.clock.Now())
	if err != nil {
		return nil, err
	}
	out := make([]telegram.SilenceSummary, 0, len(rows))
	for _, r := range rows {
		if r == nil {
			continue
		}
		out = append(out, telegram.SilenceSummary{
			ID:        r.ID,
			Pattern:   r.Pattern,
			CreatedBy: r.CreatedBy,
			ExpiresAt: r.ExpiresAt,
		})
	}
	return out, nil
}

// DeleteByPattern resolves tenant from the bot row, then forwards the
// pattern to the underlying repo. Number of affected rows propagates so
// /unsilence can craft a useful reply.
func (a silenceManagerAdapter) DeleteByPattern(ctx context.Context, botID int64, pattern string) (int64, error) {
	tenantID, err := a.tenantForBot(ctx, botID)
	if err != nil {
		return 0, err
	}
	return a.silences.DeleteByPattern(ctx, tenantID, pattern)
}

func (a silenceManagerAdapter) tenantForBot(ctx context.Context, botID int64) (int64, error) {
	bots, err := a.bots.ListAll(ctx)
	if err != nil {
		return 0, err
	}
	for _, b := range bots {
		if b != nil && b.ID == botID {
			return b.TenantID, nil
		}
	}
	return 0, domain.ErrNotFound
}

// silently consume the time import — used implicitly via clock.Now().Add.
var _ = time.Second


