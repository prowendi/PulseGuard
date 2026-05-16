package pipeline

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/tg"
)

// =====================================================================
// In-memory fakes
// =====================================================================

type fakeOutbox struct {
	mu    sync.Mutex
	items map[int64]*domain.PushOutbox
	seq   int64
	// queue ordered by NextAttemptAt; nil-safe.
	queue []int64
}

func newFakeOutbox() *fakeOutbox {
	return &fakeOutbox{items: make(map[int64]*domain.PushOutbox)}
}

func (f *fakeOutbox) Insert(ctx context.Context, item *domain.PushOutbox) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	item.ID = f.seq
	f.items[item.ID] = item
	f.queue = append(f.queue, item.ID)
	return item.ID, nil
}

func (f *fakeOutbox) ClaimNext(ctx context.Context, workerID string, now time.Time) (*domain.PushOutbox, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, id := range f.queue {
		it := f.items[id]
		if it == nil {
			continue
		}
		if it.Status != domain.OutboxPending && it.Status != domain.OutboxRetry {
			continue
		}
		if it.NextAttemptAt.After(now) {
			continue
		}
		it.Status = domain.OutboxInFlight
		it.Attempts++
		it.WorkerID = &workerID
		t := now
		it.ClaimedAt = &t
		// remove from queue
		f.queue = append(f.queue[:i], f.queue[i+1:]...)
		copy := *it
		return &copy, nil
	}
	return nil, nil
}

func (f *fakeOutbox) MarkSent(ctx context.Context, id int64, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		it.Status = domain.OutboxSent
	}
	return nil
}

func (f *fakeOutbox) MarkRetry(ctx context.Context, id int64, nextAt time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		it.Status = domain.OutboxRetry
		it.NextAttemptAt = nextAt
		r := reason
		it.LastError = &r
		f.queue = append(f.queue, id)
	}
	return nil
}

func (f *fakeOutbox) MarkDead(ctx context.Context, id int64, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if it, ok := f.items[id]; ok {
		it.Status = domain.OutboxDead
		r := reason
		it.LastError = &r
	}
	return nil
}

func (f *fakeOutbox) ReclaimInFlight(ctx context.Context, olderThan time.Time) (int64, error) {
	return 0, nil
}

func (f *fakeOutbox) get(id int64) *domain.PushOutbox {
	f.mu.Lock()
	defer f.mu.Unlock()
	it := f.items[id]
	if it == nil {
		return nil
	}
	// Return a snapshot so callers reading fields are race-safe against
	// concurrent worker tick mutations (Status, Attempts, LastError, ...).
	cp := *it
	return &cp
}

// channels / bots / templates -----------------------------------------

type fakeChannelRepo struct {
	m map[int64]*domain.Channel
}

func (r *fakeChannelRepo) Insert(ctx context.Context, c *domain.Channel) error {
	return errors.New("not implemented")
}
func (r *fakeChannelRepo) Update(ctx context.Context, c *domain.Channel) error {
	return errors.New("not implemented")
}
func (r *fakeChannelRepo) Delete(ctx context.Context, tenantID, id int64) error {
	return errors.New("not implemented")
}
func (r *fakeChannelRepo) GetByID(ctx context.Context, tenantID, id int64) (*domain.Channel, error) {
	c, ok := r.m[id]
	if !ok || c.TenantID != tenantID {
		return nil, domain.ErrNotFound
	}
	cp := *c
	return &cp, nil
}
func (r *fakeChannelRepo) GetByPushToken(ctx context.Context, t string) (*domain.Channel, error) {
	return nil, domain.ErrNotFound
}
func (r *fakeChannelRepo) ListByTenant(ctx context.Context, tenantID int64) ([]*domain.Channel, error) {
	return nil, nil
}
func (r *fakeChannelRepo) ReplaceTemplates(ctx context.Context, tenantID, channelID int64, bindings []*domain.ChannelTemplate) error {
	c, ok := r.m[channelID]
	if !ok || c.TenantID != tenantID {
		return domain.ErrNotFound
	}
	c.Templates = bindings
	return nil
}

type fakeBotRepo struct{ m map[int64]*domain.Bot }

func (r *fakeBotRepo) Insert(ctx context.Context, b *domain.Bot) error   { return errors.New("nyi") }
func (r *fakeBotRepo) Update(ctx context.Context, b *domain.Bot) error   { return errors.New("nyi") }
func (r *fakeBotRepo) Delete(ctx context.Context, t, id int64) error     { return errors.New("nyi") }
func (r *fakeBotRepo) ListByTenant(ctx context.Context, t int64) ([]*domain.Bot, error) {
	return nil, nil
}
func (r *fakeBotRepo) GetByID(ctx context.Context, tenantID, id int64) (*domain.Bot, error) {
	b, ok := r.m[id]
	if !ok || b.TenantID != tenantID {
		return nil, domain.ErrNotFound
	}
	cp := *b
	return &cp, nil
}
func (r *fakeBotRepo) ListAll(ctx context.Context) ([]*domain.Bot, error) {
	out := make([]*domain.Bot, 0, len(r.m))
	for _, b := range r.m {
		cp := *b
		out = append(out, &cp)
	}
	return out, nil
}

type fakeTplRepo struct{ m map[int64]*domain.Template }

func (r *fakeTplRepo) Insert(ctx context.Context, t *domain.Template) error  { return errors.New("nyi") }
func (r *fakeTplRepo) Update(ctx context.Context, t *domain.Template) error  { return errors.New("nyi") }
func (r *fakeTplRepo) Delete(ctx context.Context, tn, id int64) error        { return errors.New("nyi") }
func (r *fakeTplRepo) ListByTenant(ctx context.Context, t int64) ([]*domain.Template, error) {
	return nil, nil
}
func (r *fakeTplRepo) GetByID(ctx context.Context, tenantID, id int64) (*domain.Template, error) {
	t, ok := r.m[id]
	if !ok || t.TenantID != tenantID {
		return nil, domain.ErrNotFound
	}
	cp := *t
	return &cp, nil
}

// logs / dlq -----------------------------------------------------------

type fakeLogRepo struct {
	mu   sync.Mutex
	logs []*domain.PushLog
}

func (r *fakeLogRepo) Insert(ctx context.Context, l *domain.PushLog) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *l
	r.logs = append(r.logs, &cp)
	return nil
}
func (r *fakeLogRepo) ListByTenant(ctx context.Context, t int64, page, per int) ([]*domain.PushLog, int, error) {
	return nil, 0, nil
}
func (r *fakeLogRepo) ListByChannel(ctx context.Context, t, c int64, p, pp int) ([]*domain.PushLog, int, error) {
	return nil, 0, nil
}
func (r *fakeLogRepo) PurgeOlderThan(ctx context.Context, _ time.Time) (int64, error) {
	return 0, nil
}

type fakeDLQ struct {
	mu sync.Mutex
	dl []*domain.DeadLetter
}

func (r *fakeDLQ) Insert(ctx context.Context, dl *domain.DeadLetter) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *dl
	r.dl = append(r.dl, &cp)
	return nil
}
func (r *fakeDLQ) ListByTenant(ctx context.Context, t int64, p, pp int) ([]*domain.DeadLetter, int, error) {
	return nil, 0, nil
}
func (r *fakeDLQ) Replay(ctx context.Context, t, id int64) (int64, error) { return 0, nil }

// rate limiter / sender ------------------------------------------------

type fakeRL struct {
	allow bool
	err   error
}

func (r *fakeRL) Allow(ctx context.Context, ch int64, rate int) (bool, error) {
	if r.err != nil {
		return false, r.err
	}
	return r.allow, nil
}

type fakeSender struct {
	mu    sync.Mutex
	calls int
	resp  func(call int) (int64, error)
}

func (s *fakeSender) Send(ctx context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	s.mu.Lock()
	s.calls++
	c := s.calls
	s.mu.Unlock()
	return s.resp(c)
}

// =====================================================================
// Helpers
// =====================================================================

type workerFixture struct {
	outbox *fakeOutbox
	logs   *fakeLogRepo
	dlq    *fakeDLQ
	chans  *fakeChannelRepo
	bots   *fakeBotRepo
	tpls   *fakeTplRepo
	rl     *fakeRL
	clk    *domain.FakeClock
	w      *Worker
	logBuf *syncBuffer
}

// syncBuffer is a goroutine-safe wrapper around bytes.Buffer so multiple
// worker goroutines can write slog records concurrently in tests.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func newWorkerFixture(t *testing.T, sender domain.Sender, rlAllow bool) *workerFixture {
	t.Helper()
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	chans := &fakeChannelRepo{m: map[int64]*domain.Channel{
		1: {ID: 1, TenantID: 1, Name: "c", BotID: 10, ChatID: "12345", RatePerMin: 60, Enabled: true,
			Templates: []*domain.ChannelTemplate{{ChannelID: 1, TemplateID: 100, IsDefault: true}},
		},
	}}
	bots := &fakeBotRepo{m: map[int64]*domain.Bot{
		10: {ID: 10, TenantID: 1, Name: "b", BotToken: "TOKEN"},
	}}
	tpls := &fakeTplRepo{m: map[int64]*domain.Template{
		100: {ID: 100, TenantID: 1, Name: "t", ParseMode: domain.ParseMarkdownV2, Body: "Hello {{ .name }}"},
	}}
	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	deps := WorkerDeps{
		Outbox:    newFakeOutbox(),
		Channels:  chans,
		Bots:      bots,
		Templates: tpls,
		Logs:      &fakeLogRepo{},
		DLQ:       &fakeDLQ{},
		RL:        &fakeRL{allow: rlAllow},
		Sender:    sender,
		Clock:     clk,
		Logger:    logger,
	}
	cfg := WorkerCfg{WorkerID: "w1", PollInterval: 10 * time.Millisecond, MaxAttempts: 6, Backoff: DefaultBackoff()}
	w := New(deps, cfg)
	return &workerFixture{
		outbox: deps.Outbox.(*fakeOutbox),
		logs:   deps.Logs.(*fakeLogRepo),
		dlq:    deps.DLQ.(*fakeDLQ),
		chans:  chans,
		bots:   bots,
		tpls:   tpls,
		rl:     deps.RL.(*fakeRL),
		clk:    clk,
		w:      w,
		logBuf: logBuf,
	}
}

func (f *workerFixture) enqueue(t *testing.T) *domain.PushOutbox {
	t.Helper()
	now := f.clk.Now()
	item := &domain.PushOutbox{
		ChannelID:     1,
		TenantID:      1,
		PayloadJSON:   `{"name":"world"}`,
		Status:        domain.OutboxPending,
		NextAttemptAt: now,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	id, err := f.outbox.Insert(context.Background(), item)
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	item.ID = id
	return item
}

// =====================================================================
// Tests
// =====================================================================

func TestWorkerHappyPath(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) { return 42, nil }}
	f := newWorkerFixture(t, sender, true)
	item := f.enqueue(t)

	did, err := f.w.tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if !did {
		t.Fatalf("expected didWork=true")
	}
	got := f.outbox.get(item.ID)
	if got.Status != domain.OutboxSent {
		t.Fatalf("status = %q", got.Status)
	}
	if len(f.logs.logs) != 1 {
		t.Fatalf("logs count = %d", len(f.logs.logs))
	}
	if f.logs.logs[0].Status != domain.LogSent {
		t.Fatalf("log status = %q", f.logs.logs[0].Status)
	}
	if f.logs.logs[0].TGMessageID == nil || *f.logs.logs[0].TGMessageID != 42 {
		t.Fatalf("msg id missing")
	}
	if len(f.dlq.dl) != 0 {
		t.Fatalf("dlq should be empty")
	}
}

func TestWorkerTransientThenSuccess(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) {
		if call == 1 {
			return 0, &tg.APIError{Class: tg.Transient, Code: 503, Description: "Bad Gateway"}
		}
		return 7, nil
	}}
	f := newWorkerFixture(t, sender, true)
	item := f.enqueue(t)

	// First tick: transient error -> retry queued.
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	got := f.outbox.get(item.ID)
	if got.Status != domain.OutboxRetry {
		t.Fatalf("expected retry after transient, got %q", got.Status)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "Bad Gateway") {
		t.Fatalf("missing last_error")
	}
	// Advance the clock past the backoff (1s).
	f.clk.Advance(2 * time.Second)

	// Second tick: success.
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	got = f.outbox.get(item.ID)
	if got.Status != domain.OutboxSent {
		t.Fatalf("expected sent, got %q", got.Status)
	}
}

func TestWorkerTransientExhaustsToDLQ(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) {
		return 0, &tg.APIError{Class: tg.Transient, Code: 503, Description: "Bad Gateway"}
	}}
	f := newWorkerFixture(t, sender, true)
	item := f.enqueue(t)

	// We will loop until the row is dead. Each iteration we advance enough
	// time to clear the backoff window so the next tick can claim again.
	maxIters := 20
	for i := 0; i < maxIters; i++ {
		_, _ = f.w.tick(context.Background())
		got := f.outbox.get(item.ID)
		if got.Status == domain.OutboxDead {
			break
		}
		// Bump way past any backoff entry.
		f.clk.Advance(30 * time.Minute)
		if i == maxIters-1 {
			t.Fatalf("did not reach dead after %d iters; status=%q attempts=%d", maxIters, got.Status, got.Attempts)
		}
	}
	if len(f.dlq.dl) == 0 {
		t.Fatalf("expected DLQ row")
	}
	gotItem := f.outbox.get(item.ID)
	if gotItem.Status != domain.OutboxDead {
		t.Fatalf("status = %q", gotItem.Status)
	}
}

func TestWorkerPermanentClientStraightToDLQ(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) {
		return 0, &tg.APIError{Class: tg.PermanentClient, Code: 400, Description: "chat not found"}
	}}
	f := newWorkerFixture(t, sender, true)
	item := f.enqueue(t)

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := f.outbox.get(item.ID)
	if got.Status != domain.OutboxDead {
		t.Fatalf("status = %q", got.Status)
	}
	if len(f.dlq.dl) != 1 {
		t.Fatalf("dlq len = %d", len(f.dlq.dl))
	}
	if !strings.Contains(f.dlq.dl[0].LastError, "chat not found") {
		t.Fatalf("dlq err = %q", f.dlq.dl[0].LastError)
	}
}

func TestWorkerPermanentServer(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) {
		return 0, &tg.APIError{Class: tg.PermanentServer, Code: 401, Description: "Unauthorized"}
	}}
	f := newWorkerFixture(t, sender, true)
	item := f.enqueue(t)
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if f.outbox.get(item.ID).Status != domain.OutboxDead {
		t.Fatalf("not dead")
	}
}

func TestWorkerRenderFailureGoesToDLQ(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) { return 1, nil }}
	f := newWorkerFixture(t, sender, true)
	// Replace the template with a broken one.
	f.tpls.m[100] = &domain.Template{ID: 100, TenantID: 1, Name: "broken", Body: `{{ if `}
	item := f.enqueue(t)

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := f.outbox.get(item.ID)
	if got.Status != domain.OutboxDead {
		t.Fatalf("status = %q", got.Status)
	}
	if sender.calls != 0 {
		t.Fatalf("sender should not be called on render failure")
	}
	if len(f.dlq.dl) != 1 {
		t.Fatalf("dlq len = %d", len(f.dlq.dl))
	}
}

func TestWorkerRateLimited(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) { return 1, nil }}
	f := newWorkerFixture(t, sender, false) // RL denies
	item := f.enqueue(t)
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := f.outbox.get(item.ID)
	if got.Status != domain.OutboxRetry {
		t.Fatalf("status = %q", got.Status)
	}
	if got.LastError == nil || !strings.Contains(*got.LastError, "rate-limited") {
		t.Fatalf("last_error = %v", got.LastError)
	}
	if sender.calls != 0 {
		t.Fatalf("sender must not run when rate-limited")
	}
}

func TestWorkerChannelDisabled(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) { return 1, nil }}
	f := newWorkerFixture(t, sender, true)
	f.chans.m[1].Enabled = false
	item := f.enqueue(t)
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if f.outbox.get(item.ID).Status != domain.OutboxDead {
		t.Fatalf("not dead")
	}
	if len(f.dlq.dl) != 1 {
		t.Fatalf("dlq len = %d", len(f.dlq.dl))
	}
}

func TestWorkerTransientRetryAfter(t *testing.T) {
	// 429 with retry_after=7 should override our default 1s schedule.
	sender := &fakeSender{resp: func(call int) (int64, error) {
		return 0, &tg.APIError{Class: tg.Transient, Code: 429, Description: "rate", RetryAfter: 7 * time.Second}
	}}
	f := newWorkerFixture(t, sender, true)
	item := f.enqueue(t)
	now := f.clk.Now()
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	got := f.outbox.get(item.ID)
	if got.Status != domain.OutboxRetry {
		t.Fatalf("status = %q", got.Status)
	}
	want := now.Add(7 * time.Second)
	if !got.NextAttemptAt.Equal(want) {
		t.Fatalf("next_attempt_at = %s, want %s", got.NextAttemptAt, want)
	}
}

func TestWorkerIdleTickReturnsFalse(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) { return 1, nil }}
	f := newWorkerFixture(t, sender, true)
	did, err := f.w.tick(context.Background())
	if err != nil {
		t.Fatalf("tick: %v", err)
	}
	if did {
		t.Fatalf("idle tick must report didWork=false")
	}
}

func TestWorkerRunRespectsContext(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) { return 1, nil }}
	f := newWorkerFixture(t, sender, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.w.Run(ctx) }()
	time.Sleep(20 * time.Millisecond) // let the loop spin a bit
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("worker did not exit")
	}
}

func TestWorkerRunProcessesItem(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) { return 99, nil }}
	f := newWorkerFixture(t, sender, true)
	item := f.enqueue(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	doneRun := make(chan error, 1)
	go func() { doneRun <- f.w.Run(ctx) }()

	// Poll the fake outbox until the item is sent or we time out.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.outbox.get(item.ID).Status == domain.OutboxSent {
			cancel()
			<-doneRun
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-doneRun
	t.Fatalf("item not sent after 2s, status=%q", f.outbox.get(item.ID).Status)
}

// TestWorkerLoggerEmitsClaimAndSent asserts that the injected slog logger
// actually receives the key lifecycle events the audit demands (claim +
// sent). Regression guard against R1 — the worker must never go silent.
func TestWorkerLoggerEmitsClaimAndSent(t *testing.T) {
	sender := &fakeSender{resp: func(call int) (int64, error) { return 42, nil }}
	f := newWorkerFixture(t, sender, true)
	item := f.enqueue(t)
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	logs := f.logBuf.String()
	if !strings.Contains(logs, "pipeline.worker claimed") {
		t.Fatalf("expected 'pipeline.worker claimed' log, got:\n%s", logs)
	}
	if !strings.Contains(logs, "pipeline.worker sent") {
		t.Fatalf("expected 'pipeline.worker sent' log, got:\n%s", logs)
	}
	wantOutboxAttr := "outbox_id=" // slog text handler uses key=value
	if !strings.Contains(logs, wantOutboxAttr) {
		t.Fatalf("expected outbox_id attr in logs, got:\n%s", logs)
	}
	// Confirm the actual id appears somewhere.
	if !strings.Contains(logs, "outbox_id="+itoa(item.ID)) {
		t.Fatalf("expected outbox_id=%d in logs, got:\n%s", item.ID, logs)
	}
}

// TestWorkerLoggerNilSafe ensures the constructor substitutes a noop
// logger when WorkerDeps.Logger is nil — the worker must never panic.
func TestWorkerLoggerNilSafe(t *testing.T) {
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	deps := WorkerDeps{
		Outbox:    newFakeOutbox(),
		Channels:  &fakeChannelRepo{m: map[int64]*domain.Channel{}},
		Bots:      &fakeBotRepo{m: map[int64]*domain.Bot{}},
		Templates: &fakeTplRepo{m: map[int64]*domain.Template{}},
		Logs:      &fakeLogRepo{},
		DLQ:       &fakeDLQ{},
		RL:        &fakeRL{allow: true},
		Sender:    &fakeSender{resp: func(call int) (int64, error) { return 0, nil }},
		Clock:     clk,
		Logger:    nil, // intentionally nil
	}
	w := New(deps, WorkerCfg{WorkerID: "wnil", PollInterval: time.Millisecond, MaxAttempts: 6, Backoff: DefaultBackoff()})
	if _, err := w.tick(context.Background()); err != nil {
		t.Fatalf("tick with nil logger: %v", err)
	}
}

func itoa(v int64) string {
	// Avoid importing strconv just for the test helper; the worker test
	// file already pulls in fmt indirectly via testing assertions, but we
	// prefer a tiny inline implementation to keep the test deps minimal.
	if v == 0 {
		return "0"
	}
	neg := false
	if v < 0 {
		neg = true
		v = -v
	}
	var digits [20]byte
	i := len(digits)
	for v > 0 {
		i--
		digits[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
