package pipeline

import (
	"context"
	"errors"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// CleanupDeps groups the repos cleaned by the housekeeping worker.
type CleanupDeps struct {
	Logs     domain.LogRepo
	Dedup    domain.DedupRepo
	Sessions domain.SessionRepo
	Outbox   domain.OutboxRepo
	Clock    domain.Clock
}

// CleanupCfg captures the cadence + retention knobs.
//
//   - LogKeepDays       — push_logs older than this are deleted.
//   - DedupSweepInterval — how often we run DedupRepo.PurgeExpired.
//   - SessionsSweepInterval — how often we run SessionRepo.PurgeExpired
//     and LogRepo.PurgeOlderThan.
//   - InflightReclaimAfter — outbox rows stuck in_flight beyond this
//     duration are reclaimed back to retry once a minute.
type CleanupCfg struct {
	LogKeepDays           int
	DedupSweepInterval    time.Duration
	SessionsSweepInterval time.Duration
	InflightReclaimAfter  time.Duration
}

// Cleanup runs periodic maintenance: session expiry, dedup expiry, push
// log retention, and in-flight outbox reclamation.
type Cleanup struct {
	deps CleanupDeps
	cfg  CleanupCfg
}

// NewCleanup constructs a Cleanup worker. Intervals <= 0 are coerced to
// safe defaults so the worker is never silent.
func NewCleanup(deps CleanupDeps, cfg CleanupCfg) *Cleanup {
	if cfg.DedupSweepInterval <= 0 {
		cfg.DedupSweepInterval = time.Hour
	}
	if cfg.SessionsSweepInterval <= 0 {
		cfg.SessionsSweepInterval = time.Hour
	}
	if cfg.InflightReclaimAfter <= 0 {
		cfg.InflightReclaimAfter = 60 * time.Second
	}
	if cfg.LogKeepDays <= 0 {
		cfg.LogKeepDays = 30
	}
	return &Cleanup{deps: deps, cfg: cfg}
}

// Run blocks until ctx is cancelled, firing each sweep on its own ticker.
func (c *Cleanup) Run(ctx context.Context) error {
	dedupTk := time.NewTicker(c.cfg.DedupSweepInterval)
	defer dedupTk.Stop()
	sessTk := time.NewTicker(c.cfg.SessionsSweepInterval)
	defer sessTk.Stop()
	logsTk := time.NewTicker(c.cfg.SessionsSweepInterval) // reuse cadence
	defer logsTk.Stop()
	reclaimTk := time.NewTicker(time.Minute)
	defer reclaimTk.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-dedupTk.C:
			_, _ = c.SweepDedup(ctx)
		case <-sessTk.C:
			_, _ = c.SweepSessions(ctx)
		case <-logsTk.C:
			_, _ = c.SweepLogs(ctx)
		case <-reclaimTk.C:
			_, _ = c.ReclaimInflight(ctx)
		}
	}
}

// SweepDedup purges expired dedup keys. Public so callers can run a one-
// shot pass at startup if desired.
func (c *Cleanup) SweepDedup(ctx context.Context) (int64, error) {
	n, err := c.deps.Dedup.PurgeExpired(ctx, c.deps.Clock.Now())
	if err != nil && !errors.Is(err, context.Canceled) {
		return 0, err
	}
	return n, nil
}

// SweepSessions purges expired sessions.
func (c *Cleanup) SweepSessions(ctx context.Context) (int64, error) {
	n, err := c.deps.Sessions.PurgeExpired(ctx, c.deps.Clock.Now())
	if err != nil && !errors.Is(err, context.Canceled) {
		return 0, err
	}
	return n, nil
}

// SweepLogs deletes push_logs older than LogKeepDays.
func (c *Cleanup) SweepLogs(ctx context.Context) (int64, error) {
	cutoff := c.deps.Clock.Now().Add(-time.Duration(c.cfg.LogKeepDays) * 24 * time.Hour)
	n, err := c.deps.Logs.PurgeOlderThan(ctx, cutoff)
	if err != nil && !errors.Is(err, context.Canceled) {
		return 0, err
	}
	return n, nil
}

// ReclaimInflight rescues outbox rows stuck in 'in_flight' beyond
// InflightReclaimAfter (worker crash recovery).
func (c *Cleanup) ReclaimInflight(ctx context.Context) (int64, error) {
	cutoff := c.deps.Clock.Now().Add(-c.cfg.InflightReclaimAfter)
	n, err := c.deps.Outbox.ReclaimInFlight(ctx, cutoff)
	if err != nil && !errors.Is(err, context.Canceled) {
		return 0, err
	}
	return n, nil
}
