package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// recordingSenderWithOpts implements domain.SenderWithOpts so the V7-1
// worker path that detects the type assertion can be exercised. The
// last SendWithOpts call's Buttons + a flag that distinguishes
// SendWithOpts from Send are captured so tests can assert which path
// fired.
type recordingSenderWithOpts struct {
	mu          sync.Mutex
	plainSends  int
	withOpts    int
	lastButtons []domain.PushButton
	lastText    string
	editCalls   int
}

func (s *recordingSenderWithOpts) Send(_ context.Context, _, _, _, text string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.plainSends++
	s.lastText = text
	return 1, nil
}

func (s *recordingSenderWithOpts) SendWithOpts(_ context.Context, _, _, _, text string, opts domain.SendOptions) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.withOpts++
	s.lastText = text
	s.lastButtons = append([]domain.PushButton(nil), opts.Buttons...)
	return 2, nil
}

func (s *recordingSenderWithOpts) EditMessage(_ context.Context, _, _ string, _ int64, _, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.editCalls++
	return nil
}

// TestWorkerPayloadButtonsRouteThroughSendWithOpts is the V7-1
// regression net: a payload carrying _buttons must hit SendWithOpts
// (not plain Send) so the inline_keyboard reaches Telegram.
func TestWorkerPayloadButtonsRouteThroughSendWithOpts(t *testing.T) {
	sender := &recordingSenderWithOpts{}
	f := newWorkerFixture(t, sender, true)
	now := f.clk.Now()
	item := &domain.PushOutbox{
		ChannelID:     1,
		TenantID:      1,
		PayloadJSON:   `{"name":"world","_buttons":[{"text":"ACK","callback":"ack:db01-cpu"},{"text":"Runbook","url":"https://x"}]}`,
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
		t.Fatalf("status = %q want sent", got.Status)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.withOpts != 1 {
		t.Fatalf("SendWithOpts calls = %d want 1 (plainSends=%d)", sender.withOpts, sender.plainSends)
	}
	if len(sender.lastButtons) != 2 {
		t.Fatalf("buttons len = %d want 2: %+v", len(sender.lastButtons), sender.lastButtons)
	}
	if sender.lastButtons[0].Callback != "ack:db01-cpu" {
		t.Fatalf("button[0].Callback = %q", sender.lastButtons[0].Callback)
	}
	if sender.lastButtons[1].URL != "https://x" {
		t.Fatalf("button[1].URL = %q", sender.lastButtons[1].URL)
	}
}

// TestWorkerNoButtonsKeepsPlainSend ensures payloads without _buttons
// still hit the legacy Sender.Send path so the existing happy-path
// regression suite stays valid even after V7-1.
func TestWorkerNoButtonsKeepsPlainSend(t *testing.T) {
	sender := &recordingSenderWithOpts{}
	f := newWorkerFixture(t, sender, true)
	_ = f.enqueue(t)
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.plainSends != 1 {
		t.Fatalf("plainSends = %d want 1", sender.plainSends)
	}
	if sender.withOpts != 0 {
		t.Fatalf("withOpts = %d want 0 (no buttons → no opts call)", sender.withOpts)
	}
}

// TestExtractButtons_Malformed exercises the silent-skip path: text or
// callback/url missing means the entry is dropped, not crashed. Empty
// list collapses to nil so the worker takes the plain-Send branch.
func TestExtractButtons_Malformed(t *testing.T) {
	cases := map[string]string{
		"missing text":         `{"_buttons":[{"callback":"x"}]}`,
		"missing callback+url": `{"_buttons":[{"text":"x"}]}`,
		"empty list":           `{"_buttons":[]}`,
		"non-array":            `{"_buttons":"x"}`,
		"missing key":          `{}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			payload := decodePayload(raw)
			if got := extractButtons(payload); got != nil {
				t.Fatalf("expected nil for %q, got %+v", name, got)
			}
		})
	}
}

// avoid unused-import in case future imports are pruned.
var _ time.Duration

// =====================================================================
// V7-2 editMessageText state machine
// =====================================================================

// TestWorkerV72FirstPushSendsAndPersistsThread proves the V7-2
// happy-path bootstrapping: a payload carrying a fresh `_fingerprint`
// must go through Send (NOT Edit) on the first attempt and the
// resulting tg_message_id must be stamped into message_threads so the
// next push of the same fingerprint can collapse.
func TestWorkerV72FirstPushSendsAndPersistsThread(t *testing.T) {
	sender := &fakeSender{resp: func(int) (int64, error) { return 555, nil }}
	f := newWorkerFixture(t, sender, true)
	_ = f.enqueueWithPayload(t, `{"name":"world","_fingerprint":"db01-cpu"}`)
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if sender.calls != 1 {
		t.Fatalf("expected one send (bootstrapping), got %d", sender.calls)
	}
	if sender.editCalls != 0 {
		t.Fatalf("expected zero edits on bootstrap, got %d", sender.editCalls)
	}
	if f.threads.count() != 1 {
		t.Fatalf("expected one message_thread row, got %d", f.threads.count())
	}
	row, err := f.threads.GetByFingerprint(context.Background(), 1, "db01-cpu")
	if err != nil {
		t.Fatalf("GetByFingerprint: %v", err)
	}
	if row.TGMessageID != 555 {
		t.Fatalf("thread tg_message_id = %d, want 555", row.TGMessageID)
	}
	if row.ChatID != "12345" {
		t.Fatalf("thread chat_id = %q, want 12345", row.ChatID)
	}
}

// TestWorkerV72SecondPushEditsExistingThread is the load-bearing test:
// after a thread row exists, a second push of the same `_fingerprint`
// MUST hit EditMessage instead of Send. This is the whole point of
// V7-2 — collapse the alert storm into one updating Telegram message.
func TestWorkerV72SecondPushEditsExistingThread(t *testing.T) {
	sender := &fakeSender{resp: func(int) (int64, error) { return 777, nil }}
	f := newWorkerFixture(t, sender, true)

	// First push: bootstraps the thread.
	_ = f.enqueueWithPayload(t, `{"name":"world","_fingerprint":"db01-cpu"}`)
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if sender.calls != 1 || sender.editCalls != 0 {
		t.Fatalf("after first push: calls=%d edits=%d (want 1/0)", sender.calls, sender.editCalls)
	}

	// Second push: same fingerprint → must edit.
	_ = f.enqueueWithPayload(t, `{"name":"again","_fingerprint":"db01-cpu"}`)
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick2: %v", err)
	}
	if sender.calls != 1 {
		t.Fatalf("expected NO new send after edit collapse, got %d sends", sender.calls)
	}
	if sender.editCalls != 1 {
		t.Fatalf("expected one edit, got %d", sender.editCalls)
	}
	if sender.lastEditID != 777 {
		t.Fatalf("edit targeted msg_id=%d, want 777 (original send id)", sender.lastEditID)
	}
	// The rendered text must reflect the new payload (template is
	// "Hello {{ .name }}" so the second push edits to "Hello again").
	if sender.lastEditText != "Hello again" {
		t.Fatalf("edit text = %q, want %q", sender.lastEditText, "Hello again")
	}

	// And exactly one thread row stays in place (UNIQUE collapses).
	if f.threads.count() != 1 {
		t.Fatalf("expected one thread row after edit, got %d", f.threads.count())
	}
}

// TestWorkerV72NoFingerprintKeepsLegacySend is the regression net for
// the "do not break the pre-V7 path" invariant. Payloads that omit
// `_fingerprint` must NEVER touch MessageThreads — no lookup, no
// upsert. This guard is what the spec means by "behaviour unchanged
// for callers that have not opted in".
func TestWorkerV72NoFingerprintKeepsLegacySend(t *testing.T) {
	sender := &fakeSender{resp: func(int) (int64, error) { return 42, nil }}
	f := newWorkerFixture(t, sender, true)
	_ = f.enqueueWithPayload(t, `{"name":"world"}`) // no fingerprint
	if _, err := f.w.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if sender.calls != 1 {
		t.Fatalf("send count = %d, want 1", sender.calls)
	}
	if sender.editCalls != 0 {
		t.Fatalf("edit count = %d, want 0 (no fingerprint = no edit)", sender.editCalls)
	}
	if f.threads.count() != 0 {
		t.Fatalf("thread rows = %d, want 0 (no fingerprint = no thread)", f.threads.count())
	}
}

// TestExtractFingerprint covers the helper's allowable surface so a
// future refactor cannot silently change which payloads opt into the
// V7-2 state machine.
func TestExtractFingerprint(t *testing.T) {
	cases := map[string]struct {
		raw  string
		want string
	}{
		"plain":               {`{"_fingerprint":"db01-cpu"}`, "db01-cpu"},
		"trimmed":             {`{"_fingerprint":"  db01-cpu  "}`, "db01-cpu"},
		"empty string":        {`{"_fingerprint":""}`, ""},
		"missing":             {`{"name":"x"}`, ""},
		"non-string":          {`{"_fingerprint":42}`, ""},
		"null":                {`{"_fingerprint":null}`, ""},
		"object":              {`{"_fingerprint":{"k":"v"}}`, ""},
		"with other keys":     {`{"name":"x","_fingerprint":"k"}`, "k"},
		"whitespace collapse": {`{"_fingerprint":"   "}`, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := extractFingerprint(decodePayload(tc.raw))
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}