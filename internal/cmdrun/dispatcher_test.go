package cmdrun

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
	"github.com/prowendi/PulseGuard/internal/platform/telegram"
	"github.com/prowendi/PulseGuard/internal/scripting"
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
	New(nil, &scripting.Executor{}, &fakeRecorder{}, nil)
}

func TestDispatcher_NewPanicsOnNilExecutor(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil executor")
		}
	}()
	New(&fakeResolver{}, nil, &fakeRecorder{}, nil)
}

func TestDispatcher_NewPanicsOnNilRecorder(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil recorder")
		}
	}()
	New(&fakeResolver{}, &scripting.Executor{}, nil, nil)
}

func TestDispatcher_UnknownCommandSkips(t *testing.T) {
	resolver := &fakeResolver{err: domain.ErrNotFound}
	d := New(resolver, &scripting.Executor{}, &fakeRecorder{}, nil)
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
	d := New(resolver, &scripting.Executor{}, recorder, nil)

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
	d := New(resolver, &scripting.Executor{}, &fakeRecorder{}, nil)
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
	d := New(resolver, &scripting.Executor{Timeout: 200 * time.Millisecond}, recorder, nil)
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
	d := New(resolver, &scripting.Executor{}, recorder, nil)
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
	d := New(resolver, &scripting.Executor{}, &fakeRecorder{}, nil)
	_, err := d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 1, ChatID: 1, Name: "x",
	})
	if err == nil || errors.Is(err, telegram.ErrDispatchSkip) {
		t.Fatalf("expected non-skip resolver error to bubble, got %v", err)
	}
}

// ── Round-2 audit H1: failure-path logging ────────────────────────────
//
// These tests pin the contract that Recorder.Upsert failures and
// opaque Executor failures both emit structured Warns with the
// command_id + tenant_id context an operator needs to correlate the
// drop with the source command.

// TestDispatcher_NilLoggerSafe proves nil logger is replaced by a
// noop sink — the dispatcher must never panic on Upsert failure when
// the operator forgot to pass a logger.
func TestDispatcher_NilLoggerSafe(t *testing.T) {
	resolver := &fakeResolver{cmd: &domain.Command{
		ID: 1, TenantID: 7, Name: "/echo",
		Code:    "def handle(args):\n    return \"ok\"",
		Enabled: true,
	}}
	recorder := &fakeRecorder{failOn: errors.New("disk full")}
	d := New(resolver, &scripting.Executor{}, recorder, nil) // nil logger
	out, err := d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 1, ChatID: 1, Name: "echo",
	})
	if err != nil {
		t.Fatalf("Dispatch with nil logger: %v", err)
	}
	if out.Text != "ok" {
		t.Fatalf("out.Text = %q", out.Text)
	}
}

// TestDispatcher_LogsRecorderFailure asserts that an Upsert error is
// surfaced via the injected slog with tenant_id + command_id keys.
func TestDispatcher_LogsRecorderFailure(t *testing.T) {
	resolver := &fakeResolver{cmd: &domain.Command{
		ID: 42, TenantID: 7, Name: "/echo",
		Code:    "def handle(args):\n    return \"ok\"",
		Enabled: true,
	}}
	recorder := &fakeRecorder{failOn: errors.New("disk full")}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := New(resolver, &scripting.Executor{}, recorder, logger)

	out, err := d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 9, ChatID: 1234, Name: "echo",
	})
	if err != nil {
		t.Fatalf("Dispatch should not surface recorder error, got %v", err)
	}
	if out.Text != "ok" {
		t.Fatalf("out.Text = %q", out.Text)
	}
	logs := buf.String()
	if !strings.Contains(logs, "subscriber upsert failed") {
		t.Fatalf("expected 'subscriber upsert failed' log, got:\n%s", logs)
	}
	if !strings.Contains(logs, "tenant_id=7") {
		t.Fatalf("expected tenant_id=7 in logs, got:\n%s", logs)
	}
	if !strings.Contains(logs, "command_id=42") {
		t.Fatalf("expected command_id=42 in logs, got:\n%s", logs)
	}
	if !strings.Contains(logs, "disk full") {
		t.Fatalf("expected underlying error in logs, got:\n%s", logs)
	}
}

// TestDispatcher_LogsExecutorOpaqueFailure asserts the default branch
// (executor returns an error that does NOT match a sentinel) emits a
// Warn with command_id + tenant_id + truncated args so operators can
// match a "命令执行失败" report against the offending row.
func TestDispatcher_LogsExecutorOpaqueFailure(t *testing.T) {
	resolver := &fakeResolver{cmd: &domain.Command{
		ID: 99, TenantID: 3, Name: "/broken",
		Code: `
def handle(args):
    fail = undefined_symbol
    return fail
`,
		Enabled: true,
	}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := New(resolver, &scripting.Executor{}, &fakeRecorder{}, logger)

	_, err := d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 1, ChatID: 1, Name: "broken", Args: []string{"alpha", "beta"},
	})
	if err == nil {
		t.Fatalf("expected execution error to bubble through default branch")
	}
	logs := buf.String()
	if !strings.Contains(logs, "command execution failed") {
		t.Fatalf("expected 'command execution failed' log, got:\n%s", logs)
	}
	if !strings.Contains(logs, "tenant_id=3") {
		t.Fatalf("expected tenant_id=3 in logs, got:\n%s", logs)
	}
	if !strings.Contains(logs, "command_id=99") {
		t.Fatalf("expected command_id=99 in logs, got:\n%s", logs)
	}
	if !strings.Contains(logs, "alpha beta") {
		t.Fatalf("expected joined args in logs, got:\n%s", logs)
	}
}

// TestDispatcher_LogsExecutorOpaqueFailureTruncatesArgs proves the
// 256-byte cap on args so a 1 MiB pasted payload cannot blow up
// structured log shipping.
func TestDispatcher_LogsExecutorOpaqueFailureTruncatesArgs(t *testing.T) {
	resolver := &fakeResolver{cmd: &domain.Command{
		ID: 1, TenantID: 1, Name: "/x",
		Code: `
def handle(args):
    fail = undefined_symbol
    return fail
`,
		Enabled: true,
	}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	d := New(resolver, &scripting.Executor{}, &fakeRecorder{}, logger)

	huge := strings.Repeat("a", 1024)
	_, _ = d.Dispatch(context.Background(), telegram.DispatchInput{
		BotID: 1, ChatID: 1, Name: "x", Args: []string{huge},
	})
	logs := buf.String()
	if !strings.Contains(logs, "(truncated)") {
		t.Fatalf("expected '(truncated)' marker in logs, got snippet:\n%s",
			logs[:min(len(logs), 400)])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
