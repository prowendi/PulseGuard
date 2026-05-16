package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// logFixture extends outboxFixture with a LogRepo and DLQ repo.
type logFixture struct {
	*outboxFixture
	logs *LogRepo
	dlq  *DeadLetterRepo
}

func newLogFixture(t *testing.T) *logFixture {
	t.Helper()
	f := newOutboxFixture(t)
	return &logFixture{
		outboxFixture: f,
		logs:          NewLogRepo(f.db, f.clk),
		dlq:           NewDeadLetterRepo(f.db, f.clk),
	}
}

func (f *logFixture) makeLog(t *testing.T, status domain.LogStatus, attempts int) *domain.PushLog {
	t.Helper()
	l := &domain.PushLog{
		ChannelID:    f.channel.ID,
		TenantID:     f.tenant.ID,
		PayloadJSON:  `{"a":1}`,
		RenderedText: "rendered",
		Status:       status,
		Attempts:     attempts,
	}
	if err := f.logs.Insert(context.Background(), l); err != nil {
		t.Fatalf("insert log: %v", err)
	}
	return l
}

// ─── LogRepo ────────────────────────────────────────────────────────

func TestLogRepo_InsertAndPagination(t *testing.T) {
	f := newLogFixture(t)
	for i := 0; i < 30; i++ {
		f.makeLog(t, domain.LogSent, 1)
		f.clk.Advance(time.Millisecond)
	}
	got, total, err := f.logs.ListByTenant(context.Background(), f.tenant.ID, 1, 10)
	if err != nil {
		t.Fatalf("ListByTenant: %v", err)
	}
	if total != 30 {
		t.Fatalf("total = %d want 30", total)
	}
	if len(got) != 10 {
		t.Fatalf("len = %d want 10", len(got))
	}
	// newest-first ordering: first item has the latest created_at
	if !got[0].CreatedAt.After(got[1].CreatedAt) &&
		!got[0].CreatedAt.Equal(got[1].CreatedAt) {
		t.Fatalf("expected newest-first order, got %v", got[0].CreatedAt)
	}

	page2, _, err := f.logs.ListByTenant(context.Background(), f.tenant.ID, 2, 10)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 10 {
		t.Fatalf("page2 len = %d want 10", len(page2))
	}
	// no overlap
	if got[9].ID == page2[0].ID {
		t.Fatalf("page1 last id equals page2 first id: %d", got[9].ID)
	}
}

func TestLogRepo_ListByChannel_TenantScoped(t *testing.T) {
	f := newLogFixture(t)
	b := f.makeBot(t, "bot2", "x:y")
	tpl := f.makeTemplate(t, "tpl2")
	ch2 := f.makeChannel(t, "ch2", "tok2", b.ID, tpl.ID)

	// 3 logs on channel 1, 2 logs on channel 2.
	for i := 0; i < 3; i++ {
		f.makeLog(t, domain.LogSent, 1)
	}
	for i := 0; i < 2; i++ {
		l := &domain.PushLog{
			ChannelID: ch2.ID, TenantID: f.tenant.ID,
			PayloadJSON: `{}`, RenderedText: "x", Status: domain.LogFailed,
		}
		if err := f.logs.Insert(context.Background(), l); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
	_, total, err := f.logs.ListByChannel(context.Background(), f.tenant.ID, f.channel.ID, 1, 20)
	if err != nil {
		t.Fatalf("ListByChannel: %v", err)
	}
	if total != 3 {
		t.Fatalf("ch1 total = %d want 3", total)
	}
	_, total2, err := f.logs.ListByChannel(context.Background(), f.tenant.ID, ch2.ID, 1, 20)
	if err != nil {
		t.Fatalf("ListByChannel ch2: %v", err)
	}
	if total2 != 2 {
		t.Fatalf("ch2 total = %d want 2", total2)
	}
}

func TestLogRepo_PurgeOlderThan(t *testing.T) {
	f := newLogFixture(t)
	// Insert 3 old logs at clk.T.
	for i := 0; i < 3; i++ {
		f.makeLog(t, domain.LogSent, 1)
	}
	// Advance clock and add 2 fresh ones.
	f.clk.Advance(time.Hour)
	for i := 0; i < 2; i++ {
		f.makeLog(t, domain.LogSent, 1)
	}

	cutoff := f.clk.T.Add(-30 * time.Minute)
	n, err := f.logs.PurgeOlderThan(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if n != 3 {
		t.Fatalf("purged = %d want 3", n)
	}
	_, total, err := f.logs.ListByTenant(context.Background(), f.tenant.ID, 1, 50)
	if err != nil {
		t.Fatalf("List after purge: %v", err)
	}
	if total != 2 {
		t.Fatalf("remaining = %d want 2", total)
	}
}

func TestLogRepo_RejectsInvalidStatus(t *testing.T) {
	f := newLogFixture(t)
	err := f.logs.Insert(context.Background(), &domain.PushLog{
		ChannelID: f.channel.ID, TenantID: f.tenant.ID,
		PayloadJSON: "{}", RenderedText: "x", Status: "bogus",
	})
	if !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("invalid status err = %v want ErrValidation", err)
	}
}

// ─── DeadLetterRepo ─────────────────────────────────────────────────

func TestDeadLetterRepo_Insert(t *testing.T) {
	f := newLogFixture(t)
	dl := &domain.DeadLetter{
		OutboxID: 42, ChannelID: f.channel.ID, TenantID: f.tenant.ID,
		PayloadJSON: `{"k":"v"}`, LastError: "permanent", Attempts: 3,
	}
	if err := f.dlq.Insert(context.Background(), dl); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if dl.ID == 0 {
		t.Fatalf("ID unset")
	}
	if dl.CreatedAt.IsZero() {
		t.Fatalf("CreatedAt unset")
	}
}

func TestDeadLetterRepo_ListByTenant(t *testing.T) {
	f := newLogFixture(t)
	for i := 0; i < 5; i++ {
		dl := &domain.DeadLetter{
			OutboxID: int64(i + 1), ChannelID: f.channel.ID, TenantID: f.tenant.ID,
			PayloadJSON: `{}`, LastError: "x", Attempts: 1,
		}
		if err := f.dlq.Insert(context.Background(), dl); err != nil {
			t.Fatalf("Insert %d: %v", i, err)
		}
		f.clk.Advance(time.Millisecond)
	}
	got, total, err := f.dlq.ListByTenant(context.Background(), f.tenant.ID, 1, 3)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if total != 5 {
		t.Fatalf("total = %d want 5", total)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d want 3", len(got))
	}
}

func TestDeadLetterRepo_Replay(t *testing.T) {
	f := newLogFixture(t)
	dl := &domain.DeadLetter{
		OutboxID: 7, ChannelID: f.channel.ID, TenantID: f.tenant.ID,
		PayloadJSON: `{"replay":"me"}`, LastError: "bad", Attempts: 3,
	}
	if err := f.dlq.Insert(context.Background(), dl); err != nil {
		t.Fatal(err)
	}
	newID, err := f.dlq.Replay(context.Background(), f.tenant.ID, dl.ID)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if newID == 0 {
		t.Fatal("Replay returned id=0")
	}
	// New row must be claimable.
	claimed, err := f.outbox.ClaimNext(context.Background(), "w1", f.clk.T)
	if err != nil {
		t.Fatal(err)
	}
	if claimed == nil {
		t.Fatalf("replay did not produce a claimable row")
	}
	if claimed.ID != newID {
		t.Fatalf("claimed id = %d want %d", claimed.ID, newID)
	}
	if claimed.PayloadJSON != `{"replay":"me"}` {
		t.Fatalf("payload = %q", claimed.PayloadJSON)
	}
	if claimed.Attempts != 1 {
		t.Fatalf("attempts = %d want 1 (start fresh)", claimed.Attempts)
	}
}

func TestDeadLetterRepo_Replay_WrongTenant(t *testing.T) {
	f := newLogFixture(t)
	dl := &domain.DeadLetter{
		OutboxID: 1, ChannelID: f.channel.ID, TenantID: f.tenant.ID,
		PayloadJSON: `{}`, LastError: "x", Attempts: 1,
	}
	if err := f.dlq.Insert(context.Background(), dl); err != nil {
		t.Fatal(err)
	}
	_, err := f.dlq.Replay(context.Background(), 9999, dl.ID)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("wrong-tenant replay err = %v want ErrNotFound", err)
	}
}

func TestDeadLetterRepo_Replay_Missing(t *testing.T) {
	f := newLogFixture(t)
	_, err := f.dlq.Replay(context.Background(), f.tenant.ID, 9999)
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing replay err = %v want ErrNotFound", err)
	}
}
