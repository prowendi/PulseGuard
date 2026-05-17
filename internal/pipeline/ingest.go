package pipeline

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// Ingestor turns a webhook PushRequest into an outbox row, honouring the
// per-channel dedup window. The HTTP layer typically uses it once per
// inbound request.
type Ingestor struct {
	outbox domain.OutboxRepo
	dedup  *Dedup
	clock  domain.Clock
}

// NewIngestor constructs an Ingestor.
func NewIngestor(outbox domain.OutboxRepo, dedup *Dedup, clock domain.Clock) *Ingestor {
	return &Ingestor{outbox: outbox, dedup: dedup, clock: clock}
}

// Ingest writes a pending row into push_outbox unless the dedup gate
// suppresses it.
//
// Returns (outboxID, dropped, err):
//   - dropped=true, outboxID=0, err=nil  -> the fingerprint is still live
//     in the channel's window; the caller should respond
//     200 {dropped:true,reason:"dedup"} per spec §4.1.
//   - dropped=false, outboxID>0, err=nil -> a row was inserted; worker
//     will pick it up on its next tick.
//   - err != nil                          -> infrastructure failure; caller
//     should return 5xx.
func (i *Ingestor) Ingest(ctx context.Context, ch *domain.Channel, payload map[string]any, dedupKey string) (int64, bool, error) {
	if ch == nil {
		return 0, false, fmt.Errorf("%w: channel is nil", domain.ErrValidation)
	}
	if payload == nil {
		// Empty payload is acceptable (templates may not use it) but
		// must round-trip as a valid JSON object.
		payload = map[string]any{}
	}

	if ch.DedupWindowS > 0 {
		fp := Fingerprint(payload, dedupKey)
		if fp != "" {
			drop, err := i.dedup.ShouldDrop(ctx, ch.ID, fp, ch.DedupWindowS)
			if err != nil {
				return 0, false, fmt.Errorf("dedup: %w", err)
			}
			if drop {
				return 0, true, nil
			}
		}
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, false, fmt.Errorf("marshal payload: %w", err)
	}

	item := &domain.PushOutbox{
		ChannelID:     ch.ID,
		TenantID:      ch.TenantID,
		PayloadJSON:   string(raw),
		Status:        domain.OutboxPending,
		NextAttemptAt: i.clock.Now(),
	}
	if dedupKey != "" {
		dk := dedupKey
		item.DedupKey = &dk
	}
	id, err := i.outbox.Insert(ctx, item)
	if err != nil {
		return 0, false, fmt.Errorf("insert outbox: %w", err)
	}
	return id, false, nil
}
