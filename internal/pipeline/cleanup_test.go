package pipeline

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// -----------------------------------------------------------------
// Fakes for cleanup tests.
// -----------------------------------------------------------------

type fakeSessionRepo struct {
	mu           sync.Mutex
	purgedBefore time.Time
	purgedN      int64
}

func (r *fakeSessionRepo) Insert(ctx context.Context, s *domain.Session) error {
	return errors.New("nyi")
}
func (r *fakeSessionRepo) GetByID(ctx context.Context, id string) (*domain.Session, error) {
	return nil, domain.ErrNotFound
}
func (r *fakeSessionRepo) Delete(ctx context.Context, id string) error { return nil }
func (r *fakeSessionRepo) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgedBefore = now
	r.purgedN++
	return r.purgedN, nil
}

type fakeDedupRepo struct {
	mu           sync.Mutex
	purgedBefore time.Time
	calls        int
}

func (r *fakeDedupRepo) SeenOrInsert(ctx context.Context, channelID int64, fp string, now time.Time, w int) (bool, error) {
	return false, nil
}
func (r *fakeDedupRepo) PurgeExpired(ctx context.Context, now time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgedBefore = now
	r.calls++
	return int64(r.calls), nil
}

type fakeLogRepoForCleanup struct {
	mu           sync.Mutex
	purgedBefore time.Time
	calls        int
}

func (r *fakeLogRepoForCleanup) Insert(ctx context.Context, l *domain.PushLog) error { return nil }
func (r *fakeLogRepoForCleanup) ListByTenant(ctx context.Context, t int64, p, pp int) ([]*domain.PushLog, int, error) {
	return nil, 0, nil
}
func (r *fakeLogRepoForCleanup) ListByChannel(ctx context.Context, t, c int64, p, pp int) ([]*domain.PushLog, int, error) {
	return nil, 0, nil
}
func (r *fakeLogRepoForCleanup) PurgeOlderThan(ctx context.Context, t time.Time) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.purgedBefore = t
	r.calls++
	return int64(r.calls), nil
}

type fakeOutboxForCleanup struct {
	mu           sync.Mutex
	reclaimedBefore time.Time
	calls           int
}

func (f *fakeOutboxForCleanup) Insert(ctx context.Context, it *domain.PushOutbox) (int64, error) {
	return 0, errors.New("nyi")
}
func (f *fakeOutboxForCleanup) ClaimNext(ctx context.Context, w string, now time.Time) (*domain.PushOutbox, error) {
	return nil, nil
}
func (f *fakeOutboxForCleanup) MarkSent(ctx context.Context, id int64, now time.Time) error { return nil }
func (f *fakeOutboxForCleanup) MarkRetry(ctx context.Context, id int64, nextAt time.Time, reason string) error {
	return nil
}
func (f *fakeOutboxForCleanup) MarkDead(ctx context.Context, id int64, reason string) error { return nil }
func (f *fakeOutboxForCleanup) ReclaimInFlight(ctx context.Context, older time.Time) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reclaimedBefore = older
	f.calls++
	return int64(f.calls), nil
}

// -----------------------------------------------------------------

func newCleanupFixture(t *testing.T, cfg CleanupCfg) (*Cleanup, *fakeSessionRepo, *fakeDedupRepo, *fakeLogRepoForCleanup, *fakeOutboxForCleanup, *domain.FakeClock) {
	t.Helper()
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	sess := &fakeSessionRepo{}
	dedup := &fakeDedupRepo{}
	logs := &fakeLogRepoForCleanup{}
	outbox := &fakeOutboxForCleanup{}
	deps := CleanupDeps{
		Logs:     logs,
		Dedup:    dedup,
		Sessions: sess,
		Outbox:   outbox,
		Clock:    clk,
	}
	c := NewCleanup(deps, cfg)
	return c, sess, dedup, logs, outbox, clk
}

func TestCleanupSweepDedup(t *testing.T) {
	c, _, dedup, _, _, clk := newCleanupFixture(t, CleanupCfg{LogKeepDays: 7})
	n, err := c.SweepDedup(context.Background())
	if err != nil {
		t.Fatalf("SweepDedup: %v", err)
	}
	if n != 1 {
		t.Fatalf("n = %d", n)
	}
	if !dedup.purgedBefore.Equal(clk.Now()) {
		t.Fatalf("dedup cutoff = %s, want %s", dedup.purgedBefore, clk.Now())
	}
}

func TestCleanupSweepSessions(t *testing.T) {
	c, sess, _, _, _, clk := newCleanupFixture(t, CleanupCfg{LogKeepDays: 7})
	n, err := c.SweepSessions(context.Background())
	if err != nil {
		t.Fatalf("SweepSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("n = %d", n)
	}
	if !sess.purgedBefore.Equal(clk.Now()) {
		t.Fatalf("sess cutoff = %s, want %s", sess.purgedBefore, clk.Now())
	}
}

func TestCleanupSweepLogsUsesRetention(t *testing.T) {
	c, _, _, logs, _, clk := newCleanupFixture(t, CleanupCfg{LogKeepDays: 7})
	if _, err := c.SweepLogs(context.Background()); err != nil {
		t.Fatalf("SweepLogs: %v", err)
	}
	want := clk.Now().Add(-7 * 24 * time.Hour)
	if !logs.purgedBefore.Equal(want) {
		t.Fatalf("logs cutoff = %s, want %s", logs.purgedBefore, want)
	}
}

func TestCleanupReclaimInflight(t *testing.T) {
	c, _, _, _, outbox, clk := newCleanupFixture(t, CleanupCfg{
		LogKeepDays:          7,
		InflightReclaimAfter: 90 * time.Second,
	})
	if _, err := c.ReclaimInflight(context.Background()); err != nil {
		t.Fatalf("ReclaimInflight: %v", err)
	}
	want := clk.Now().Add(-90 * time.Second)
	if !outbox.reclaimedBefore.Equal(want) {
		t.Fatalf("cutoff = %s, want %s", outbox.reclaimedBefore, want)
	}
}

func TestCleanupRunRespectsContext(t *testing.T) {
	c, _, _, _, _, _ := newCleanupFixture(t, CleanupCfg{
		LogKeepDays:           1,
		DedupSweepInterval:    10 * time.Millisecond,
		SessionsSweepInterval: 10 * time.Millisecond,
		InflightReclaimAfter:  10 * time.Second,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	time.Sleep(30 * time.Millisecond) // a couple of ticks
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Cleanup.Run did not exit")
	}
}

func TestCleanupDefaultsApplied(t *testing.T) {
	c := NewCleanup(CleanupDeps{}, CleanupCfg{})
	if c.cfg.DedupSweepInterval <= 0 {
		t.Fatalf("dedup interval default")
	}
	if c.cfg.SessionsSweepInterval <= 0 {
		t.Fatalf("sessions interval default")
	}
	if c.cfg.InflightReclaimAfter <= 0 {
		t.Fatalf("inflight default")
	}
	if c.cfg.LogKeepDays != 30 {
		t.Fatalf("log keep default = %d", c.cfg.LogKeepDays)
	}
}

// TestCleanupRunFiresEachSweepDeterministically is the R13 regression
// guard: with the injected TickerSource fakes we can drive each sweep
// loop without time.Sleep and verify that PurgeExpired /
// PurgeOlderThan / ReclaimInFlight all get invoked exactly when the
// corresponding fake channel fires.
//
// Refs: code-review-report C-I6.
func TestCleanupRunFiresEachSweepDeterministically(t *testing.T) {
	// Fakes: four named channels, one per ticker call site. The
	// TickerSource closure picks the next one in declaration order.
	dedupCh := make(chan time.Time, 1)
	sessCh := make(chan time.Time, 1)
	logsCh := make(chan time.Time, 1)
	reclaimCh := make(chan time.Time, 1)
	channels := []chan time.Time{dedupCh, sessCh, logsCh, reclaimCh}
	idx := 0
	ticker := func(d time.Duration) (<-chan time.Time, func()) {
		ch := channels[idx]
		idx++
		return ch, func() {}
	}

	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	sess := &fakeSessionRepo{}
	dedup := &fakeDedupRepo{}
	logs := &fakeLogRepoForCleanup{}
	outbox := &fakeOutboxForCleanup{}
	c := NewCleanup(CleanupDeps{
		Logs:     logs,
		Dedup:    dedup,
		Sessions: sess,
		Outbox:   outbox,
		Clock:    clk,
		Ticker:   ticker,
	}, CleanupCfg{
		LogKeepDays:           7,
		DedupSweepInterval:    time.Hour,
		SessionsSweepInterval: time.Hour,
		InflightReclaimAfter:  90 * time.Second,
	})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()

	// Fire each ticker exactly once. The Run loop iterates one case at
	// a time so we use a tiny eventually-poll on the fake state to know
	// when each sweep has finished before firing the next.
	fire := func(label string, ch chan time.Time, observed func() int) {
		t.Helper()
		before := observed()
		ch <- clk.Now()
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if observed() > before {
				return
			}
			runtimeGosched()
		}
		t.Fatalf("%s sweep did not run after ticker fire", label)
	}
	fire("dedup", dedupCh, func() int {
		dedup.mu.Lock()
		defer dedup.mu.Unlock()
		return dedup.calls
	})
	fire("sessions", sessCh, func() int {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return int(sess.purgedN)
	})
	fire("logs", logsCh, func() int {
		logs.mu.Lock()
		defer logs.mu.Unlock()
		return logs.calls
	})
	fire("reclaim", reclaimCh, func() int {
		outbox.mu.Lock()
		defer outbox.mu.Unlock()
		return outbox.calls
	})

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Cleanup.Run did not exit")
	}

	// Final sanity: every sweep recorded exactly one call.
	if got := func() int { sess.mu.Lock(); defer sess.mu.Unlock(); return int(sess.purgedN) }(); got != 1 {
		t.Fatalf("sessions calls = %d", got)
	}
	if got := func() int { dedup.mu.Lock(); defer dedup.mu.Unlock(); return dedup.calls }(); got != 1 {
		t.Fatalf("dedup calls = %d", got)
	}
	if got := func() int { logs.mu.Lock(); defer logs.mu.Unlock(); return logs.calls }(); got != 1 {
		t.Fatalf("logs calls = %d", got)
	}
	if got := func() int { outbox.mu.Lock(); defer outbox.mu.Unlock(); return outbox.calls }(); got != 1 {
		t.Fatalf("reclaim calls = %d", got)
	}
}

// runtimeGosched is a tiny indirection so the deterministic test does
// not import "runtime" at the top of the file.
func runtimeGosched() { time.Sleep(time.Millisecond) }
