package cmdrun

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/platform/telegram"
	"github.com/wendi/pulseguard/internal/scripting"
)

// fakeResolver returns a static (cmd, err) pair keyed by name.
type fakeResolver struct {
	cmd  *domain.Command
	err  error
	last string
}

func (f *fakeResolver) GetByBotAndName(_ context.Context, _ int64, name string) (*domain.Command, error) {
	f.last = name
	if f.err != nil {
		return nil, f.err
	}
	if f.cmd == nil {
		return nil, domain.ErrNotFound
	}
	return f.cmd, nil
}

// fakeRecorder records every upsert.
type fakeRecorder struct {
	mu     sync.Mutex
	items  []*domain.Subscriber
	failOn error
}

func (f *fakeRecorder) Upsert(_ context.Context, s *domain.Subscriber) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, s)
	return f.failOn
}

func TestDispatcher_NewPanicsOnNil(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil resolver")
		}
	}()
	New(nil, &scripting.Executor{}, &fakeRecorder{})
}

func TestDispatcher_NewPanicsOnNilExecutor(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil executor")
		}
	}()
	New(&fakeResolver{}, nil, &fakeRecorder{})
}

func TestDispatcher_NewPanicsOnNilRecorder(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil recorder")
		}
	}()
	New(&fakeResolver{}, &scripting.Executor{}, nil)
}

func TestDispatcher_UnknownCommandSkips(t *testing.T) {
	resolver := &fakeResolver{err: domain.ErrNotFound}
	d := New(resolver, &scripting.Executor{}, &fakeRecorder{})
	_, err := d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 1, ChatID: 1, Name: "missing",
	})
	if !errors.Is(err, telegram.ErrDispatchSkip) {
		t.Fatalf("err = %v, want ErrDispatchSkip", err)
	}
}

func TestDispatcher_ExecutesAndStitches(t *testing.T) {
	resolver := &fakeResolver{cmd: &domain.Command{
		ID: 1, TenantID: 7, Name: "/echo",
		Code: `
def handle(args):
    print("hi")
    return ":".join(args)
`,
		Enabled: true,
	}}
	recorder := &fakeRecorder{}
	d := New(resolver, &scripting.Executor{}, recorder)

	out, err := d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 9, ChatID: 42, Name: "echo", Args: []string{"a", "b"},
	})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(out.Text, "hi") || !strings.Contains(out.Text, "a:b") {
		t.Fatalf("out.Text missing print or return: %q", out.Text)
	}

	// Subscriber upserted with correct fields.
	if len(recorder.items) != 1 {
		t.Fatalf("recorder len=%d", len(recorder.items))
	}
	sub := recorder.items[0]
	if sub.TenantID != 7 || sub.CommandID != 1 || sub.BotID != 9 || sub.ChatID != "42" || sub.Platform != domain.PlatformTelegram {
		t.Fatalf("subscriber mismatch: %+v", sub)
	}
}

func TestDispatcher_TriesBothSlashAndBareName(t *testing.T) {
	resolver := &fakeResolver{
		// First call (with "/echo") returns NotFound; second call
		// (with "echo") would succeed. fakeResolver does not switch
		// on name, so we just confirm slash is tried first.
		err: domain.ErrNotFound,
	}
	d := New(resolver, &scripting.Executor{}, &fakeRecorder{})
	_, _ = d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 1, ChatID: 1, Name: "echo",
	})
	// Last attempt would be "echo" (after "/echo" failed).
	if resolver.last != "echo" {
		t.Fatalf("last lookup = %q want bare-name fallback", resolver.last)
	}
}

func TestDispatcher_TimeoutBecomesErrDispatchTimeout(t *testing.T) {
	resolver := &fakeResolver{cmd: &domain.Command{
		ID: 1, TenantID: 7, Name: "/slow",
		Code: `
def loop(n):
    for i in range(n):
        for j in range(n):
            pass
def handle(args):
    loop(20000)
    return "done"
`,
		Enabled: true,
	}}
	recorder := &fakeRecorder{}
	d := New(resolver, &scripting.Executor{Timeout: 200 * time.Millisecond}, recorder)
	_, err := d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 1, ChatID: 1, Name: "slow",
	})
	if !errors.Is(err, telegram.ErrDispatchTimeout) {
		t.Fatalf("err = %v, want ErrDispatchTimeout", err)
	}
}

func TestDispatcher_RecorderErrorNonFatal(t *testing.T) {
	resolver := &fakeResolver{cmd: &domain.Command{
		ID: 1, TenantID: 7, Name: "/echo",
		Code:    `def handle(args):` + "\n" + `    return "ok"`,
		Enabled: true,
	}}
	recorder := &fakeRecorder{failOn: errors.New("disk full")}
	d := New(resolver, &scripting.Executor{}, recorder)
	out, err := d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 1, ChatID: 1, Name: "echo",
	})
	if err != nil {
		t.Fatalf("Dispatch should not surface recorder error, got %v", err)
	}
	if out.Text != "ok" {
		t.Fatalf("out.Text = %q", out.Text)
	}
}

func TestDispatcher_ResolverErrorOtherThanNotFoundBubbles(t *testing.T) {
	resolver := &fakeResolver{err: errors.New("db down")}
	d := New(resolver, &scripting.Executor{}, &fakeRecorder{})
	_, err := d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 1, ChatID: 1, Name: "x",
	})
	if err == nil || errors.Is(err, telegram.ErrDispatchSkip) {
		t.Fatalf("expected non-skip resolver error to bubble, got %v", err)
	}
}
