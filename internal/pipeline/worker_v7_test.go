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