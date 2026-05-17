package pipeline

import (
	"context"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// Dedup is a thin wrapper around domain.DedupRepo + domain.Clock so the
// ingest layer can ask "should I drop this push?" without leaking the
// repo internals.
type Dedup struct {
	repo  domain.DedupRepo
	clock domain.Clock
}

// NewDedup constructs a Dedup gate.
func NewDedup(repo domain.DedupRepo, clock domain.Clock) *Dedup {
	return &Dedup{repo: repo, clock: clock}
}

// ShouldDrop returns true when channelID+fp has been seen within windowSec
// seconds — the caller must abandon the current push. windowSec<=0
// disables deduplication (returns false unconditionally).
func (d *Dedup) ShouldDrop(ctx context.Context, channelID int64, fp string, windowSec int) (bool, error) {
	if windowSec <= 0 {
		return false, nil
	}
	return d.repo.SeenOrInsert(ctx, channelID, fp, d.clock.Now(), windowSec)
}
