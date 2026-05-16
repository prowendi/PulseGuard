package store

import (
	"context"
	"sync"
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

// TestDedupRepo_ConcurrentUpsertCounts4x100 fires 4 goroutines each
// calling SeenOrInsert 100 times with the same (channel, fingerprint).
// Post-conditions: exactly one caller observes alreadySeen=false (the
// first sighting); hit_count converges to 400. Regression guard against
// the deferred-tx race the prior SELECT-then-INSERT path could exhibit.
func TestDedupRepo_ConcurrentUpsertCounts4x100(t *testing.T) {
	dr, f := dedupFixture(t)
	ctx := context.Background()
	const goroutines = 4
	const perGoroutine = 100

	var firstSightings int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				seen, err := dr.SeenOrInsert(ctx, f.channel.ID, "race-fp", f.clk.T, 600)
				if err != nil {
					t.Errorf("SeenOrInsert: %v", err)
					return
				}
				if !seen {
					mu.Lock()
					firstSightings++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	if firstSightings != 1 {
		t.Fatalf("first-sighting count = %d, want exactly 1", firstSightings)
	}
	var hits int
	if err := f.db.QueryRow(
		`SELECT hit_count FROM dedup_keys WHERE channel_id = ? AND fingerprint = ?`,
		f.channel.ID, "race-fp",
	).Scan(&hits); err != nil {
		t.Fatalf("scan hits: %v", err)
	}
	if want := goroutines * perGoroutine; hits != want {
		t.Fatalf("hit_count = %d, want %d", hits, want)
	}
}

// TestRateLimitRepo_ConcurrentAllowExactlyRate verifies that under
// concurrent contention exactly ratePerMin Allow calls return true.
// BEGIN IMMEDIATE serialises writers so the bucket cannot be drained
// past zero.
func TestRateLimitRepo_ConcurrentAllowExactlyRate(t *testing.T) {
	rl, f := rlFixture(t)
	ctx := context.Background()
	const rate = 20
	const goroutines = 8
	const perGoroutine = 10 // 80 attempts total against capacity 20

	var allowed int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				ok, err := rl.Allow(ctx, f.channel.ID, rate)
				if err != nil {
					t.Errorf("Allow: %v", err)
					return
				}
				if ok {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	if allowed != rate {
		t.Fatalf("allowed = %d, want exactly %d (capacity)", allowed, rate)
	}
	// Bucket must end at 0 (or just under) — no time advanced so no refill.
	var tokens float64
	if err := f.db.QueryRow(
		`SELECT tokens FROM rate_buckets WHERE channel_id = ?`, f.channel.ID,
	).Scan(&tokens); err != nil {
		t.Fatalf("scan tokens: %v", err)
	}
	if tokens > 0.001 {
		t.Fatalf("tokens left = %f, want ~0", tokens)
	}
}

// TestDedupRepo_ExpiryEqualsNowIsExpired pins the boundary semantics
// (covers test-coverage-report T-H3). With windowSec=10 and clock
// advanced exactly 10s, the row must be considered expired.
func TestDedupRepo_ExpiryEqualsNowIsExpired(t *testing.T) {
	dr, f := dedupFixture(t)
	if _, err := dr.SeenOrInsert(context.Background(), f.channel.ID, "edge", f.clk.T, 10); err != nil {
		t.Fatal(err)
	}
	atExpiry := f.clk.T.Add(10 * time.Second)
	seen, err := dr.SeenOrInsert(context.Background(), f.channel.ID, "edge", atExpiry, 10)
	if err != nil {
		t.Fatal(err)
	}
	if seen {
		t.Fatalf("at exactly expires_at the window should already be expired (got seen=true)")
	}

	// Just before expiry, the row is still live.
	dr2, f2 := dedupFixture(t)
	if _, err := dr2.SeenOrInsert(context.Background(), f2.channel.ID, "edge", f2.clk.T, 10); err != nil {
		t.Fatal(err)
	}
	atAlmost := f2.clk.T.Add(10*time.Second - time.Millisecond)
	seen, err = dr2.SeenOrInsert(context.Background(), f2.channel.ID, "edge", atAlmost, 10)
	if err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatalf("just before expiry the row must still be live (got seen=false)")
	}
}
