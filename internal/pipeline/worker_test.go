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
func (r *fakeBotRepo) SetEnabled(ctx context.Context, tenantID, id int64, enabled bool) error {
	b, ok := r.m[id]
	if !ok || b.TenantID != tenantID {
		return domain.ErrNotFound
	}
	b.Enabled = enabled
	return nil
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

// fakeMessageThreadRepo is an in-memory domain.MessageThreadRepo used
// by V7-2 worker tests. Threads are keyed by (channel_id, fingerprint)
// to match the SQL UNIQUE. nil-safe for tests that pre-date V7-2: the
// worker is wired with this fake on every fixture so the edit branch
// is always reachable.
type fakeMessageThreadRepo struct {
	mu      sync.Mutex
	threads map[string]*domain.MessageThread
	seq     int64
	// upsertErr / lookupErr, when non-nil, force the corresponding op
	// to fail — used by tests that exercise the non-fatal fallback
	// paths (lookup failure must NOT DLQ; upsert failure must NOT
	// derail the send).
	upsertErr error
	lookupErr error
}

func newFakeMessageThreadRepo() *fakeMessageThreadRepo {
	return &fakeMessageThreadRepo{threads: map[string]*domain.MessageThread{}}
}

func threadKey(channelID int64, fp string) string {
	return strconvI(channelID) + "|" + fp
}

// strconvI is a tiny int64→string so the test file does not pull in
// strconv (mirrors itoa below).
func strconvI(v int64) string { return itoa(v) }

func (r *fakeMessageThreadRepo) Upsert(ctx context.Context, m *domain.MessageThread) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.upsertErr != nil {
		return r.upsertErr
	}
	k := threadKey(m.ChannelID, m.Fingerprint)
	if existing, ok := r.threads[k]; ok {
		existing.ChatID = m.ChatID
		existing.TGMessageID = m.TGMessageID
		m.ID = existing.ID
		return nil
	}
	r.seq++
	m.ID = r.seq
	cp := *m
	r.threads[k] = &cp
	return nil
}

func (r *fakeMessageThreadRepo) GetByFingerprint(ctx context.Context, channelID int64, fp string) (*domain.MessageThread, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lookupErr != nil {
		return nil, r.lookupErr
	}
	if m, ok := r.threads[threadKey(channelID, fp)]; ok {
		cp := *m
		return &cp, nil
	}
	return nil, domain.ErrNotFound
}

func (r *fakeMessageThreadRepo) DeleteByChannel(ctx context.Context, tenantID, channelID int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, m := range r.threads {
		if m.ChannelID == channelID && m.TenantID == tenantID {
			delete(r.threads, k)
		}
	}
	return nil
}

func (r *fakeMessageThreadRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.threads)
}

// fakeSilenceRepo is an in-memory domain.SilenceRepo. The implementation
// is deliberately tiny — V7-3 worker tests only need Insert/Match. Other
// methods return nil so they satisfy the interface.
type fakeSilenceRepo struct {
	mu      sync.Mutex
	rows    []*domain.Silence
	seq     int64
	matchErr error
}

func newFakeSilenceRepo() *fakeSilenceRepo { return &fakeSilenceRepo{} }

func (r *fakeSilenceRepo) Insert(ctx context.Context, s *domain.Silence) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	s.ID = r.seq
	cp := *s
	r.rows = append(r.rows, &cp)
	return nil
}

func (r *fakeSilenceRepo) ListActive(ctx context.Context, tenantID int64, now time.Time) ([]*domain.Silence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []*domain.Silence
	for _, s := range r.rows {
		if s.TenantID == tenantID && !now.After(s.ExpiresAt) {
			cp := *s
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (r *fakeSilenceRepo) Delete(ctx context.Context, tenantID, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, s := range r.rows {
		if s.TenantID == tenantID && s.ID == id {
			r.rows = append(r.rows[:i], r.rows[i+1:]...)
			return nil
		}
	}
	return domain.ErrNotFound
}

func (r *fakeSilenceRepo) DeleteByPattern(ctx context.Context, tenantID int64, pattern string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var keep []*domain.Silence
	var n int64
	for _, s := range r.rows {
		if s.TenantID == tenantID && s.Pattern == pattern {
			n++
			continue
		}
		keep = append(keep, s)
	}
	r.rows = keep
	return n, nil
}

func (r *fakeSilenceRepo) Match(ctx context.Context, tenantID int64, fingerprint string, now time.Time) (bool, error) {
	if r.matchErr != nil {
		return false, r.matchErr
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if fingerprint == "" {
		return false, nil
	}
	for _, s := range r.rows {
		if s.TenantID != tenantID {
			continue
		}
		if now.After(s.ExpiresAt) {
			continue
		}
		if strings.HasPrefix(fingerprint, s.Pattern) {
			return true, nil
		}
	}
	return false, nil
}

func (r *fakeSilenceRepo) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.rows)
}

type fakeSender struct {
	mu    sync.Mutex
	calls int
	resp  func(call int) (int64, error)
	// V7-2: edit tracking — every editFn lookup records the call so
	// tests can assert which path the worker took. nil resp returns
	// nil so the legacy "send-only" tests can ignore Edit entirely.
	editCalls    int
	lastEditID   int64
	lastEditText string
	editResp     func(call int) error
}

func (s *fakeSender) Send(ctx context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	s.mu.Lock()
	s.calls++
	c := s.calls
	s.mu.Unlock()
	return s.resp(c)
}

// SendWithOpts is needed so fakeSender satisfies domain.SenderWithOpts
// — without it the V7-2 worker would never take the editMessageText
// branch in tests because the type assertion would fall through. The
// implementation delegates to Send so legacy tests that only set resp
// continue to see exactly one send-per-call.
func (s *fakeSender) SendWithOpts(ctx context.Context, botToken, chatID, parseMode, text string, _ domain.SendOptions) (int64, error) {
	return s.Send(ctx, botToken, chatID, parseMode, text)
}

// EditMessage records the edit and returns whatever editResp dictates.
// When editResp is nil the call succeeds — that's the friendly default
// for happy-path tests; failure-path tests inject their own editResp.
func (s *fakeSender) EditMessage(ctx context.Context, botToken, chatID string, messageID int64, parseMode, text string) error {
	s.mu.Lock()
	s.editCalls++
	c := s.editCalls
	s.lastEditID = messageID
	s.lastEditText = text
	s.mu.Unlock()
	if s.editResp != nil {
		return s.editResp(c)
	}
	return nil
}

// =====================================================================
// Helpers
// =====================================================================

type workerFixture struct {
	outbox   *fakeOutbox
	logs     *fakeLogRepo
	dlq      *fakeDLQ
	chans    *fakeChannelRepo
	bots     *fakeBotRepo
	tpls     *fakeTplRepo
	rl       *fakeRL
	threads  *fakeMessageThreadRepo
	silences *fakeSilenceRepo
	clk      *domain.FakeClock
	w        *Worker
	logBuf   *syncBuffer
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
	threads := newFakeMessageThreadRepo()
	silences := newFakeSilenceRepo()
	deps := WorkerDeps{
		Outbox:         newFakeOutbox(),
		Channels:       chans,
		Bots:           bots,
		Templates:      tpls,
		Logs:           &fakeLogRepo{},
		DLQ:            &fakeDLQ{},
		RL:             &fakeRL{allow: rlAllow},
		Sender:         sender,
		Clock:          clk,
		MessageThreads: threads,
		Silences:       silences,
		Logger:         logger,
	}
	cfg := WorkerCfg{WorkerID: "w1", PollInterval: 10 * time.Millisecond, MaxAttempts: 6, Backoff: DefaultBackoff()}
	w := New(deps, cfg)
	return &workerFixture{
		outbox:   deps.Outbox.(*fakeOutbox),
		logs:     deps.Logs.(*fakeLogRepo),
		dlq:      deps.DLQ.(*fakeDLQ),
		chans:    chans,
		bots:     bots,
		tpls:     tpls,
		rl:       deps.RL.(*fakeRL),
		threads:  threads,
		silences: silences,
		clk:      clk,
		w:        w,
		logBuf:   logBuf,
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

// =====================================================================
// Multi-template fixture & tests (R2-5)
// =====================================================================
//
// Above fixtures bind exactly one default template per channel; the
// audit's H5/Q2 finding is that pickTemplateID's "caller picked a
// specific template_id" branch is uncovered. Helpers below build a
// channel with two bindings (one default, one non-default) so we can
// exercise the three pickTemplateID outcomes:
//   1. payload selects the non-default binding → render with it
//   2. payload omits _template_id → render with the default binding
//   3. payload selects a template_id NOT bound to the channel → DLQ

// newMultiTemplateFixture is identical to newWorkerFixture but with
// two templates bound to channel 1: template 100 default ("Hello…"),
// template 200 non-default ("Bye…"). The sender records the rendered
// text so each test can assert which body was sent.
func newMultiTemplateFixture(t *testing.T, sender domain.Sender) *workerFixture {
	t.Helper()
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	chans := &fakeChannelRepo{m: map[int64]*domain.Channel{
		1: {ID: 1, TenantID: 1, Name: "c", BotID: 10, ChatID: "12345", RatePerMin: 60, Enabled: true,
			Templates: []*domain.ChannelTemplate{
				{ChannelID: 1, TemplateID: 100, IsDefault: true, SortOrder: 0},
				{ChannelID: 1, TemplateID: 200, IsDefault: false, SortOrder: 1},
			},
		},
	}}
	bots := &fakeBotRepo{m: map[int64]*domain.Bot{
		10: {ID: 10, TenantID: 1, Name: "b", BotToken: "TOKEN"},
	}}
	tpls := &fakeTplRepo{m: map[int64]*domain.Template{
		100: {ID: 100, TenantID: 1, Name: "default", ParseMode: domain.ParseMarkdownV2, Body: "Hello {{ .name }}"},
		200: {ID: 200, TenantID: 1, Name: "second", ParseMode: domain.ParseMarkdownV2, Body: "Bye {{ .name }}"},
	}}
	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	threads := newFakeMessageThreadRepo()
	silences := newFakeSilenceRepo()
	deps := WorkerDeps{
		Outbox:         newFakeOutbox(),
		Channels:       chans,
		Bots:           bots,
		Templates:      tpls,
		Logs:           &fakeLogRepo{},
		DLQ:            &fakeDLQ{},
		RL:             &fakeRL{allow: true},
		Sender:         sender,
		Clock:          clk,
		MessageThreads: threads,
		Silences:       silences,
		Logger:         logger,
	}
	cfg := WorkerCfg{WorkerID: "w1", PollInterval: 10 * time.Millisecond, MaxAttempts: 6, Backoff: DefaultBackoff()}
	w := New(deps, cfg)
	return &workerFixture{
		outbox:   deps.Outbox.(*fakeOutbox),
		logs:     deps.Logs.(*fakeLogRepo),
		dlq:      deps.DLQ.(*fakeDLQ),
		chans:    chans,
		bots:     bots,
		tpls:     tpls,
		rl:       deps.RL.(*fakeRL),
		threads:  threads,
		silences: silences,
		clk:      clk,
		w:        w,
		logBuf:   logBuf,
	}
}

// recordingSender captures the rendered text on each Send so multi-
// template tests can assert which template body was selected.
type recordingSender struct {
	mu    sync.Mutex
	texts []string
}

func (s *recordingSender) Send(_ context.Context, _, _, _, text string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.texts = append(s.texts, text)
	return int64(len(s.texts)), nil
}

func (s *recordingSender) lastText() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.texts) == 0 {
		return ""
	}
	return s.texts[len(s.texts)-1]
}

// TestWorkerPicksTemplateByIDFromPayload covers pickTemplateID
// precedence rule #1: payload["_template_id"] points to a non-default
// binding; the worker must honour it and render that template.
func TestWorkerPicksTemplateByIDFromPayload(t *testing.T) {
	sender := &recordingSender{}
	f := newMultiTemplateFixture(t, sender)
	// payload selects template 200 (non-default Bye…).
	now := f.clk.Now()
	item := &domain.PushOutbox{
		ChannelID:     1,
		TenantID:      1,
		PayloadJSON:   `{"name":"world","_template_id":200}`,
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

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.outbox.get(item.ID); got.Status != domain.OutboxSent {
		t.Fatalf("status = %q, want sent", got.Status)
	}
	if text := sender.lastText(); !strings.HasPrefix(text, "Bye ") {
		t.Fatalf("sender saw %q, want Bye-prefixed (template 200)", text)
	}
	if len(f.dlq.dl) != 0 {
		t.Fatalf("dlq should be empty, got %d", len(f.dlq.dl))
	}
}

// TestWorkerFallsBackToDefaultWhenNoTemplateID covers precedence rule
// #3: payload omits _template_id even though the channel has more than
// one binding; the default (IsDefault=true) template must win.
func TestWorkerFallsBackToDefaultWhenNoTemplateID(t *testing.T) {
	sender := &recordingSender{}
	f := newMultiTemplateFixture(t, sender)
	now := f.clk.Now()
	item := &domain.PushOutbox{
		ChannelID:     1,
		TenantID:      1,
		PayloadJSON:   `{"name":"world"}`, // no _template_id
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

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.outbox.get(item.ID); got.Status != domain.OutboxSent {
		t.Fatalf("status = %q, want sent", got.Status)
	}
	if text := sender.lastText(); !strings.HasPrefix(text, "Hello ") {
		t.Fatalf("sender saw %q, want Hello-prefixed (default template 100)", text)
	}
}

// TestWorkerDLQsWhenTemplateIDNotBound proves the audit's strict
// behaviour: a caller specifying _template_id that is NOT bound to
// the channel must NOT silently fall back to the default — the row is
// DLQ'd so the operator notices the mismatch. Without this guarantee
// a caller could ask for template X and silently receive Y, which is
// a multi-tenant correctness hazard.
func TestWorkerDLQsWhenTemplateIDNotBound(t *testing.T) {
	sender := &recordingSender{}
	f := newMultiTemplateFixture(t, sender)
	// Template 999 exists nowhere in the tpls map nor in the
	// channel's bindings — the worker must reject it.
	now := f.clk.Now()
	item := &domain.PushOutbox{
		ChannelID:     1,
		TenantID:      1,
		PayloadJSON:   `{"name":"world","_template_id":999}`,
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

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.outbox.get(item.ID); got.Status != domain.OutboxDead {
		t.Fatalf("status = %q, want dead", got.Status)
	}
	if len(sender.texts) != 0 {
		t.Fatalf("sender must not run when template_id is unbound (got %d sends)", len(sender.texts))
	}
	if len(f.dlq.dl) != 1 {
		t.Fatalf("dlq len = %d, want 1", len(f.dlq.dl))
	}
	if !strings.Contains(f.dlq.dl[0].LastError, "999") {
		t.Fatalf("dlq err = %q, want mention of requested template_id 999", f.dlq.dl[0].LastError)
	}
}

// TestWorkerDLQsWhenNoDefaultTemplate covers the H6 audit gap:
// channel exists, has zero IsDefault bindings (i.e. all bindings have
// IsDefault=false — a state that should be prevented by the unique
// partial index but can happen if migrations race). Worker must DLQ
// rather than panic or send nothing.
func TestWorkerDLQsWhenNoDefaultTemplate(t *testing.T) {
	sender := &recordingSender{}
	f := newMultiTemplateFixture(t, sender)
	// Demote both bindings so IsDefault is false everywhere.
	f.chans.m[1].Templates[0].IsDefault = false
	now := f.clk.Now()
	item := &domain.PushOutbox{
		ChannelID:     1,
		TenantID:      1,
		PayloadJSON:   `{"name":"world"}`, // no _template_id → default lookup
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

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.outbox.get(item.ID); got.Status != domain.OutboxDead {
		t.Fatalf("status = %q, want dead", got.Status)
	}
	if len(sender.texts) != 0 {
		t.Fatalf("sender must not run with no default binding (got %d sends)", len(sender.texts))
	}
	if len(f.dlq.dl) != 1 {
		t.Fatalf("dlq len = %d, want 1", len(f.dlq.dl))
	}
	if !strings.Contains(f.dlq.dl[0].LastError, "template binding") {
		t.Fatalf("dlq err = %q, want mention of missing template binding", f.dlq.dl[0].LastError)
	}
}

// =====================================================================
// Condition-based auto-routing (V3.B1+)
// =====================================================================

// newConditionFixture binds three templates to channel 1:
//   - 100 (default, empty condition, SortOrder=10)
//   - 200 (Condition `level eq critical`, SortOrder=0)
//   - 300 (Condition `value gt 90`, SortOrder=1)
//
// Bindings are intentionally returned in SortOrder ASC order — exactly
// how channel_repo.loadBindings hydrates the slice — so pickTemplateID
// walks them in the same order the production code sees.
func newConditionFixture(t *testing.T, sender domain.Sender) *workerFixture {
	t.Helper()
	clk := &domain.FakeClock{T: time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)}
	chans := &fakeChannelRepo{m: map[int64]*domain.Channel{
		1: {ID: 1, TenantID: 1, Name: "c", BotID: 10, ChatID: "12345", RatePerMin: 60, Enabled: true,
			Templates: []*domain.ChannelTemplate{
				{ChannelID: 1, TemplateID: 200, IsDefault: false, SortOrder: 0, Condition: "level eq critical"},
				{ChannelID: 1, TemplateID: 300, IsDefault: false, SortOrder: 1, Condition: "value gt 90"},
				{ChannelID: 1, TemplateID: 100, IsDefault: true, SortOrder: 10, Condition: ""},
			},
		},
	}}
	bots := &fakeBotRepo{m: map[int64]*domain.Bot{
		10: {ID: 10, TenantID: 1, Name: "b", BotToken: "TOKEN"},
	}}
	tpls := &fakeTplRepo{m: map[int64]*domain.Template{
		100: {ID: 100, TenantID: 1, Name: "default", ParseMode: domain.ParseMarkdownV2, Body: "Default {{ .name }}"},
		200: {ID: 200, TenantID: 1, Name: "critical", ParseMode: domain.ParseMarkdownV2, Body: "CRIT {{ .name }}"},
		300: {ID: 300, TenantID: 1, Name: "highval", ParseMode: domain.ParseMarkdownV2, Body: "HIGH {{ .name }}"},
	}}
	logBuf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	threads := newFakeMessageThreadRepo()
	silences := newFakeSilenceRepo()
	deps := WorkerDeps{
		Outbox:         newFakeOutbox(),
		Channels:       chans,
		Bots:           bots,
		Templates:      tpls,
		Logs:           &fakeLogRepo{},
		DLQ:            &fakeDLQ{},
		RL:             &fakeRL{allow: true},
		Sender:         sender,
		Clock:          clk,
		MessageThreads: threads,
		Silences:       silences,
		Logger:         logger,
	}
	cfg := WorkerCfg{WorkerID: "w1", PollInterval: 10 * time.Millisecond, MaxAttempts: 6, Backoff: DefaultBackoff()}
	w := New(deps, cfg)
	return &workerFixture{
		outbox:   deps.Outbox.(*fakeOutbox),
		logs:     deps.Logs.(*fakeLogRepo),
		dlq:      deps.DLQ.(*fakeDLQ),
		chans:    chans,
		bots:     bots,
		tpls:     tpls,
		rl:       deps.RL.(*fakeRL),
		threads:  threads,
		silences: silences,
		clk:      clk,
		w:        w,
		logBuf:   logBuf,
	}
}

// enqueueWithPayload writes a fresh outbox row carrying the supplied
// payload JSON so condition tests can drive the worker with whatever
// fields they need without copy-pasting outbox boilerplate.
func (f *workerFixture) enqueueWithPayload(t *testing.T, payloadJSON string) *domain.PushOutbox {
	t.Helper()
	now := f.clk.Now()
	item := &domain.PushOutbox{
		ChannelID:     1,
		TenantID:      1,
		PayloadJSON:   payloadJSON,
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

// TestWorkerConditionEqHits proves the condition `level eq critical`
// routes the payload to template 200 even though template 100 is the
// channel default — the auto-route MUST win over the default fallback.
func TestWorkerConditionEqHits(t *testing.T) {
	sender := &recordingSender{}
	f := newConditionFixture(t, sender)
	item := f.enqueueWithPayload(t, `{"name":"world","level":"critical"}`)

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.outbox.get(item.ID); got.Status != domain.OutboxSent {
		t.Fatalf("status = %q want sent", got.Status)
	}
	if text := sender.lastText(); !strings.HasPrefix(text, "CRIT ") {
		t.Fatalf("text = %q want CRIT-prefixed (template 200)", text)
	}
}

// TestWorkerConditionMissFallsBackToDefault: no binding's condition
// matches, so the default (template 100) handles the payload.
func TestWorkerConditionMissFallsBackToDefault(t *testing.T) {
	sender := &recordingSender{}
	f := newConditionFixture(t, sender)
	item := f.enqueueWithPayload(t, `{"name":"world","level":"info","value":5}`)

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.outbox.get(item.ID); got.Status != domain.OutboxSent {
		t.Fatalf("status = %q want sent", got.Status)
	}
	if text := sender.lastText(); !strings.HasPrefix(text, "Default ") {
		t.Fatalf("text = %q want Default-prefixed (template 100)", text)
	}
}

// TestWorkerConditionSortOrderWins exercises multi-condition routing:
// payload satisfies BOTH `level eq critical` (binding 200, SortOrder 0)
// AND `value gt 90` (binding 300, SortOrder 1). The lower SortOrder
// must win so 200 is the chosen template.
func TestWorkerConditionSortOrderWins(t *testing.T) {
	sender := &recordingSender{}
	f := newConditionFixture(t, sender)
	item := f.enqueueWithPayload(t, `{"name":"world","level":"critical","value":99}`)

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.outbox.get(item.ID); got.Status != domain.OutboxSent {
		t.Fatalf("status = %q want sent", got.Status)
	}
	if text := sender.lastText(); !strings.HasPrefix(text, "CRIT ") {
		t.Fatalf("text = %q want CRIT (sort_order 0), not HIGH", text)
	}
}

// TestWorkerConditionNumericHits hits the second binding only — proves
// the iteration continues past a non-matching condition and that gt is
// evaluated numerically against JSON float64.
func TestWorkerConditionNumericHits(t *testing.T) {
	sender := &recordingSender{}
	f := newConditionFixture(t, sender)
	item := f.enqueueWithPayload(t, `{"name":"world","value":95}`)

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.outbox.get(item.ID); got.Status != domain.OutboxSent {
		t.Fatalf("status = %q want sent", got.Status)
	}
	if text := sender.lastText(); !strings.HasPrefix(text, "HIGH ") {
		t.Fatalf("text = %q want HIGH-prefixed (template 300)", text)
	}
}

// TestWorkerExplicitTemplateIDBypassesConditions confirms that an
// explicit _template_id in the payload short-circuits condition
// evaluation entirely — the caller's choice wins even if a condition
// would otherwise route somewhere else.
func TestWorkerExplicitTemplateIDBypassesConditions(t *testing.T) {
	sender := &recordingSender{}
	f := newConditionFixture(t, sender)
	// Payload satisfies `level eq critical` (would route to 200) but
	// caller explicitly demands template 100 (the default).
	item := f.enqueueWithPayload(t, `{"name":"world","level":"critical","_template_id":100}`)

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.outbox.get(item.ID); got.Status != domain.OutboxSent {
		t.Fatalf("status = %q want sent", got.Status)
	}
	if text := sender.lastText(); !strings.HasPrefix(text, "Default ") {
		t.Fatalf("text = %q want Default — explicit _template_id must win over condition match", text)
	}
}

// TestWorkerMalformedConditionSkipped: a binding with a garbage
// condition string must NOT crash the worker — it is skipped, and the
// next valid binding (or the default) handles the payload.
func TestWorkerMalformedConditionSkipped(t *testing.T) {
	sender := &recordingSender{}
	f := newConditionFixture(t, sender)
	// Replace the first binding's condition with garbage. The second
	// binding's `value gt 90` still matches, so template 300 should
	// fire — proving the malformed condition was skipped, not crashed.
	f.chans.m[1].Templates[0].Condition = "this is not a condition"
	item := f.enqueueWithPayload(t, `{"name":"world","value":99}`)

	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := f.outbox.get(item.ID); got.Status != domain.OutboxSent {
		t.Fatalf("status = %q want sent", got.Status)
	}
	if text := sender.lastText(); !strings.HasPrefix(text, "HIGH ") {
		t.Fatalf("text = %q want HIGH (malformed cond skipped, value gt 90 still matched)", text)
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
