package store

import (
	"context"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

func dedupFixture(t *testing.T) (*DedupRepo, *outboxFixture) {
	t.Helper()
	f := newOutboxFixture(t)
	return NewDedupRepo(f.db), f
}

func TestDedupRepo_FirstSightingMisses(t *testing.T) {
	dr, f := dedupFixture(t)
	seen, err := dr.SeenOrInsert(context.Background(), f.channel.ID, "fp1", f.clk.T, 60)
	if err != nil {
		t.Fatalf("SeenOrInsert: %v", err)
	}
	if seen {
		t.Fatalf("first sighting should not be seen")
	}
}

func TestDedupRepo_SecondSightingHits(t *testing.T) {
	dr, f := dedupFixture(t)
	if _, err := dr.SeenOrInsert(context.Background(), f.channel.ID, "fp1", f.clk.T, 60); err != nil {
		t.Fatal(err)
	}
	seen, err := dr.SeenOrInsert(context.Background(), f.channel.ID, "fp1", f.clk.T.Add(time.Second), 60)
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatalf("second sighting within window should hit")
	}

	// Bump should be reflected in hit_count
	var hits int
	if err := f.db.QueryRow(
		`SELECT hit_count FROM dedup_keys WHERE channel_id = ? AND fingerprint = ?`,
		f.channel.ID, "fp1",
	).Scan(&hits); err != nil {
		t.Fatal(err)
	}
	if hits != 2 {
		t.Fatalf("hit_count = %d want 2", hits)
	}
}

func TestDedupRepo_AfterExpiryMisses(t *testing.T) {
	dr, f := dedupFixture(t)
	if _, err := dr.SeenOrInsert(context.Background(), f.channel.ID, "fp1", f.clk.T, 10); err != nil {
		t.Fatal(err)
	}
	// 11s later → expired
	seen, err := dr.SeenOrInsert(context.Background(), f.channel.ID, "fp1", f.clk.T.Add(11*time.Second), 10)
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatalf("after expiry should miss")
	}
}

func TestDedupRepo_WindowZeroBypasses(t *testing.T) {
	dr, f := dedupFixture(t)
	seen, err := dr.SeenOrInsert(context.Background(), f.channel.ID, "fp1", f.clk.T, 0)
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatalf("window=0 should always miss")
	}
	// No row should have been written.
	var n int
	if err := f.db.QueryRow(`SELECT COUNT(*) FROM dedup_keys`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("dedup_keys rows = %d want 0", n)
	}
}

func TestDedupRepo_PurgeExpired(t *testing.T) {
	dr, f := dedupFixture(t)
	if _, err := dr.SeenOrInsert(context.Background(), f.channel.ID, "a", f.clk.T, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := dr.SeenOrInsert(context.Background(), f.channel.ID, "b", f.clk.T, 600); err != nil {
		t.Fatal(err)
	}
	// 10s later, only "a" should be expired.
	n, err := dr.PurgeExpired(context.Background(), f.clk.T.Add(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("purged = %d want 1", n)
	}
}

// ─── RateLimit ──────────────────────────────────────────────────────

func rlFixture(t *testing.T) (*RateLimitRepo, *outboxFixture) {
	t.Helper()
	f := newOutboxFixture(t)
	return NewRateLimitRepo(f.db, f.clk), f
}

func TestRateLimitRepo_AllowUnlimitedWhenRateZero(t *testing.T) {
	rl, f := rlFixture(t)
	for i := 0; i < 1000; i++ {
		ok, err := rl.Allow(context.Background(), f.channel.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("rate=0 should never deny (iter %d)", i)
		}
	}
}

func TestRateLimitRepo_ExhaustsCapacityThenRefills(t *testing.T) {
	rl, f := rlFixture(t)
	const rate = 5
	ctx := context.Background()

	// Drain capacity: first call inserts bucket with tokens=rate-1 + allow.
	for i := 0; i < rate; i++ {
		ok, err := rl.Allow(ctx, f.channel.ID, rate)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("iter %d should pass", i)
		}
	}
	// Next call: bucket empty.
	ok, err := rl.Allow(ctx, f.channel.ID, rate)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatalf("over-capacity call must be denied")
	}

	// Advance 1 minute → bucket fully refills.
	f.clk.Advance(time.Minute)
	for i := 0; i < rate; i++ {
		ok, err := rl.Allow(ctx, f.channel.ID, rate)
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			t.Fatalf("after refill iter %d should pass", i)
		}
	}
}

func TestRateLimitRepo_PartialRefill(t *testing.T) {
	rl, f := rlFixture(t)
	const rate = 60 // 1 token / second
	ctx := context.Background()

	// Drain.
	for i := 0; i < rate; i++ {
		if ok, err := rl.Allow(ctx, f.channel.ID, rate); err != nil || !ok {
			t.Fatalf("drain iter %d ok=%v err=%v", i, ok, err)
		}
	}
	if ok, _ := rl.Allow(ctx, f.channel.ID, rate); ok {
		t.Fatalf("expected deny after drain")
	}

	// Advance 10 seconds → 10 tokens regenerated.
	f.clk.Advance(10 * time.Second)
	for i := 0; i < 10; i++ {
		if ok, err := rl.Allow(ctx, f.channel.ID, rate); err != nil || !ok {
			t.Fatalf("partial refill iter %d ok=%v err=%v", i, ok, err)
		}
	}
	if ok, _ := rl.Allow(ctx, f.channel.ID, rate); ok {
		t.Fatalf("11th call after 10s should deny")
	}
}

func TestRateLimitRepo_FirstCallCreatesBucket(t *testing.T) {
	rl, f := rlFixture(t)
	if ok, err := rl.Allow(context.Background(), f.channel.ID, 5); err != nil || !ok {
		t.Fatalf("first call ok=%v err=%v", ok, err)
	}
	var n int
	if err := f.db.QueryRow(
		`SELECT COUNT(*) FROM rate_buckets WHERE channel_id = ?`, f.channel.ID,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("buckets = %d want 1", n)
	}
}

func TestRateLimitRepo_InterfaceCompliance(t *testing.T) {
	var _ domain.RateLimiter = (*RateLimitRepo)(nil)
	_ = (*RateLimitRepo)(nil)
}
