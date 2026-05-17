package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// outboxFixture extends resourceFixture with an OutboxRepo plus a ready
// channel so push_outbox.channel_id FK is satisfied.
type outboxFixture struct {
	*resourceFixture
	outbox  *OutboxRepo
	channel *domain.Channel
}

func newOutboxFixture(t *testing.T) *outboxFixture {
	t.Helper()
	f := newResourceFixture(t)
	b := f.makeBot(t, "bot", "1:t")
	tpl := f.makeTemplate(t, "tpl")
	ch := f.makeChannel(t, "ch", "tok", b.ID, tpl.ID)
	return &outboxFixture{
		resourceFixture: f,
		outbox:          NewOutboxRepo(f.db, f.clk),
		channel:         ch,
	}
}

func (f *outboxFixture) insert(t *testing.T, payload string) *domain.PushOutbox {
	t.Helper()
	item := &domain.PushOutbox{
		ChannelID:   f.channel.ID,
		TenantID:    f.tenant.ID,
		PayloadJSON: payload,
	}
	id, err := f.outbox.Insert(context.Background(), item)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id == 0 {
		t.Fatalf("Insert returned id=0")
	}
	return item
}

// readRowStatus shortcut to fetch current status + attempts.
func (f *outboxFixture) readRowStatus(t *testing.T, id int64) (domain.OutboxStatus, int) {
	t.Helper()
	var status string
	var attempts int
	if err := f.db.QueryRow(
		`SELECT status, attempts FROM push_outbox WHERE id = ?`, id,
	).Scan(&status, &attempts); err != nil {
		t.Fatalf("scan status: %v", err)
	}
	return domain.OutboxStatus(status), attempts
}

func TestOutboxRepo_InsertAndClaim(t *testing.T) {
	f := newOutboxFixture(t)
	item := f.insert(t, `{"x":1}`)

	got, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if got == nil {
		t.Fatalf("ClaimNext returned nil; expected item id=%d", item.ID)
	}
	if got.ID != item.ID {
		t.Fatalf("claimed id=%d want %d", got.ID, item.ID)
	}
	if got.Status != domain.OutboxInFlight {
		t.Fatalf("status = %q want in_flight", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts = %d want 1", got.Attempts)
	}
	if got.WorkerID == nil || *got.WorkerID != "w1" {
		t.Fatalf("worker_id = %v want w1", got.WorkerID)
	}
}

func TestOutboxRepo_ClaimNext_EmptyReturnsNil(t *testing.T) {
	f := newOutboxFixture(t)
	got, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T)
	if err != nil {
		t.Fatalf("ClaimNext: %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// TestOutboxRepo_ClaimNext_Concurrent is the gold-standard concurrency
// check: 1 row, N workers race for it; exactly one must win.
func TestOutboxRepo_ClaimNext_Concurrent(t *testing.T) {
	const workers = 8

	f := newOutboxFixture(t)
	item := f.insert(t, `{"k":"contend"}`)

	var winners atomic.Int64
	var nilResults atomic.Int64
	var errs atomic.Int64

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			got, err := f.outbox.ClaimNext(context.Background(), fmt.Sprintf("w%d", i), f.clk.T)
			if err != nil {
				errs.Add(1)
				return
			}
			if got == nil {
				nilResults.Add(1)
				return
			}
			if got.ID != item.ID {
				errs.Add(1)
				return
			}
			winners.Add(1)
		}(i)
	}
	close(start)
	wg.Wait()

	if e := errs.Load(); e != 0 {
		t.Fatalf("unexpected errors: %d", e)
	}
	if w := winners.Load(); w != 1 {
		t.Fatalf("winners = %d want exactly 1", w)
	}
	if n := nilResults.Load(); n != int64(workers-1) {
		t.Fatalf("nilResults = %d want %d", n, workers-1)
	}

	// final state: row remains in_flight with attempts=1
	st, attempts := f.readRowStatus(t, item.ID)
	if st != domain.OutboxInFlight {
		t.Fatalf("final status = %q want in_flight", st)
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d want 1 (claim must be atomic)", attempts)
	}
}

func TestOutboxRepo_MarkSent(t *testing.T) {
	f := newOutboxFixture(t)
	item := f.insert(t, `{"x":1}`)
	if _, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T); err != nil {
		t.Fatal(err)
	}
	if err := f.outbox.MarkSent(context.Background(), item.ID, f.clk.T.Add(time.Second)); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	st, _ := f.readRowStatus(t, item.ID)
	if st != domain.OutboxSent {
		t.Fatalf("status = %q want sent", st)
	}

	// ClaimNext should not pick it up again.
	again, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("ClaimNext returned %+v; sent rows must not be claimed", again)
	}
}

func TestOutboxRepo_MarkRetry_DelaysNextClaim(t *testing.T) {
	f := newOutboxFixture(t)
	item := f.insert(t, `{"x":1}`)
	if _, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T); err != nil {
		t.Fatal(err)
	}
	future := f.clk.T.Add(10 * time.Second)
	if err := f.outbox.MarkRetry(context.Background(), item.ID, future, "boom"); err != nil {
		t.Fatalf("MarkRetry: %v", err)
	}

	// Before the scheduled time → no claim.
	got, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("expected nil before next_attempt_at, got %+v", got)
	}

	// At/after the scheduled time → claimable, attempts bumped to 2.
	got, err = f.outbox.ClaimNext(context.Background(), "w1", future)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatalf("expected claim at retry time")
	}
	if got.Attempts != 2 {
		t.Fatalf("attempts = %d want 2", got.Attempts)
	}
}

func TestOutboxRepo_MarkDead_NotReclaimed(t *testing.T) {
	f := newOutboxFixture(t)
	item := f.insert(t, `{"x":1}`)
	if _, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T); err != nil {
		t.Fatal(err)
	}
	if err := f.outbox.MarkDead(context.Background(), item.ID, "perm"); err != nil {
		t.Fatalf("MarkDead: %v", err)
	}
	got, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("ClaimNext returned %+v; dead rows must stay dead", got)
	}
}

func TestOutboxRepo_ReclaimInFlight(t *testing.T) {
	f := newOutboxFixture(t)
	old := f.insert(t, `{"a":1}`)
	fresh := f.insert(t, `{"b":2}`)

	// claim both; both go in_flight with claimed_at = clk.T
	if _, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T); err != nil {
		t.Fatal(err)
	}
	if _, err := f.outbox.ClaimNext(context.Background(), "w2", f.clk.T); err != nil {
		t.Fatal(err)
	}

	// Advance clock 5 minutes. Reclaim rows claimed before clk.T-60s
	// (cutoff = clk.T + 5min - 60s = clk.T + 4min). Both rows claimed
	// at the original clk.T should be older than the cutoff.
	f.clk.Advance(5 * time.Minute)
	cutoff := f.clk.T.Add(-60 * time.Second)
	n, err := f.outbox.ReclaimInFlight(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("ReclaimInFlight: %v", err)
	}
	if n != 2 {
		t.Fatalf("reclaimed = %d want 2", n)
	}

	// Both rows must be back to retry, picklable.
	for _, id := range []int64{old.ID, fresh.ID} {
		st, _ := f.readRowStatus(t, id)
		if st != domain.OutboxRetry {
			t.Fatalf("row %d status = %q want retry", id, st)
		}
	}
}

func TestOutboxRepo_ReclaimInFlight_RespectsCutoff(t *testing.T) {
	f := newOutboxFixture(t)
	f.insert(t, `{"a":1}`)
	if _, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T); err != nil {
		t.Fatal(err)
	}
	// Advance only 30s; nothing should reclaim with a 60s cutoff.
	f.clk.Advance(30 * time.Second)
	cutoff := f.clk.T.Add(-60 * time.Second)
	n, err := f.outbox.ReclaimInFlight(context.Background(), cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("reclaimed = %d want 0", n)
	}
}

func TestOutboxRepo_Insert_RejectsBlanks(t *testing.T) {
	f := newOutboxFixture(t)
	cases := []*domain.PushOutbox{
		{TenantID: f.tenant.ID, PayloadJSON: "x"},
		{ChannelID: f.channel.ID, PayloadJSON: "x"},
		{ChannelID: f.channel.ID, TenantID: f.tenant.ID},
	}
	for i, c := range cases {
		_, err := f.outbox.Insert(context.Background(), c)
		if !errors.Is(err, domain.ErrValidation) {
			t.Fatalf("case %d err = %v want ErrValidation", i, err)
		}
	}
}

func TestOutboxRepo_Insert_RespectsCustomNextAttemptAt(t *testing.T) {
	f := newOutboxFixture(t)
	future := f.clk.T.Add(time.Minute)
	item := &domain.PushOutbox{
		ChannelID:     f.channel.ID,
		TenantID:      f.tenant.ID,
		PayloadJSON:   `{}`,
		NextAttemptAt: future,
	}
	if _, err := f.outbox.Insert(context.Background(), item); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	// Not yet due.
	got, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("scheduled future, but claimed immediately")
	}
	// At due time.
	got, err = f.outbox.ClaimNext(context.Background(), "w1", future)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatalf("expected claim at scheduled time")
	}
}
