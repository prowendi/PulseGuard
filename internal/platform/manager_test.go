package platform

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// fakeListener blocks until ctx is cancelled. Optionally returns a
// pre-set error from Run so callers can simulate permanent failures.
type fakeListener struct {
	id        string
	started   chan struct{} // closed when Run begins
	exitErr   error
	mu        sync.Mutex
	runCalled int32
}

func newFakeListener(id string) *fakeListener {
	return &fakeListener{id: id, started: make(chan struct{})}
}

func (l *fakeListener) Run(ctx context.Context) error {
	atomic.StoreInt32(&l.runCalled, 1)
	close(l.started)
	<-ctx.Done()
	l.mu.Lock()
	err := l.exitErr
	l.mu.Unlock()
	if err != nil {
		return err
	}
	return ctx.Err()
}

// errorListener returns a non-context-cancelled error from Run after a
// short delay so the Manager logs the "exited with error" branch.
type errorListener struct {
	err     error
	started chan struct{}
}

func newErrorListener(err error) *errorListener {
	return &errorListener{err: err, started: make(chan struct{})}
}

func (l *errorListener) Run(_ context.Context) error {
	close(l.started)
	return l.err
}

// panicListener panics on Run so the Manager's recover() path is exercised.
type panicListener struct{ started chan struct{} }

func newPanicListener() *panicListener { return &panicListener{started: make(chan struct{})} }

func (l *panicListener) Run(_ context.Context) error {
	close(l.started)
	panic("intentional test panic")
}

// fakeFactory builds fakeListeners on demand. Records the bot pointers
// it received so tests can assert wiring.
type fakeFactory struct {
	platform string
	mu       sync.Mutex
	built    []*domain.Bot
	supplier func(*domain.Bot) (Listener, error)
}

func (f *fakeFactory) Platform() string { return f.platform }

func (f *fakeFactory) Build(b *domain.Bot) (Listener, error) {
	f.mu.Lock()
	f.built = append(f.built, b)
	f.mu.Unlock()
	if f.supplier != nil {
		return f.supplier(b)
	}
	return newFakeListener(b.Name), nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func waitClosed(t *testing.T, ch chan struct{}, msg string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out: %s", msg)
	}
}

// eventually polls cond up to timeout, sleeping briefly between calls.
func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition never satisfied")
}

func TestManager_StartAndIsRunning(t *testing.T) {
	listener := newFakeListener("alpha")
	factory := &fakeFactory{
		platform: domain.PlatformTelegram,
		supplier: func(*domain.Bot) (Listener, error) { return listener, nil },
	}
	mgr := NewManager(quietLogger(), factory)
	t.Cleanup(mgr.Shutdown)

	bot := &domain.Bot{ID: 1, TenantID: 1, Name: "alpha", Platform: domain.PlatformTelegram}
	if err := mgr.Start(context.Background(), bot); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitClosed(t, listener.started, "listener should start")

	if !mgr.IsRunning(1) {
		t.Fatalf("IsRunning(1) = false; expected true")
	}

	mgr.Stop(1)
	if mgr.IsRunning(1) {
		t.Fatalf("IsRunning(1) = true after Stop")
	}
}

func TestManager_StartIdempotentReplacesListener(t *testing.T) {
	first := newFakeListener("first")
	second := newFakeListener("second")
	calls := 0
	factory := &fakeFactory{
		platform: domain.PlatformTelegram,
		supplier: func(*domain.Bot) (Listener, error) {
			calls++
			if calls == 1 {
				return first, nil
			}
			return second, nil
		},
	}
	mgr := NewManager(quietLogger(), factory)
	t.Cleanup(mgr.Shutdown)

	bot := &domain.Bot{ID: 7, TenantID: 1, Name: "x", Platform: domain.PlatformTelegram}
	if err := mgr.Start(context.Background(), bot); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	waitClosed(t, first.started, "first listener should start")

	if err := mgr.Start(context.Background(), bot); err != nil {
		t.Fatalf("second Start: %v", err)
	}
	waitClosed(t, second.started, "second listener should start")

	// First must have been cancelled by the swap: Run exits when ctx
	// done, and the Manager removes it from the map. Eventually the
	// map only contains the second goroutine.
	eventually(t, 2*time.Second, func() bool { return mgr.IsRunning(7) })

	// Both listeners had Run called.
	if atomic.LoadInt32(&first.runCalled) != 1 {
		t.Fatal("first listener Run was not called")
	}
	if atomic.LoadInt32(&second.runCalled) != 1 {
		t.Fatal("second listener Run was not called")
	}
}

func TestManager_Shutdown(t *testing.T) {
	l1 := newFakeListener("a")
	l2 := newFakeListener("b")
	calls := 0
	factory := &fakeFactory{
		platform: domain.PlatformTelegram,
		supplier: func(*domain.Bot) (Listener, error) {
			calls++
			if calls == 1 {
				return l1, nil
			}
			return l2, nil
		},
	}
	mgr := NewManager(quietLogger(), factory)
	_ = mgr.Start(context.Background(), &domain.Bot{ID: 1, TenantID: 1, Name: "a", Platform: domain.PlatformTelegram})
	_ = mgr.Start(context.Background(), &domain.Bot{ID: 2, TenantID: 1, Name: "b", Platform: domain.PlatformTelegram})
	waitClosed(t, l1.started, "l1 start")
	waitClosed(t, l2.started, "l2 start")

	mgr.Shutdown()
	if mgr.IsRunning(1) || mgr.IsRunning(2) {
		t.Fatal("IsRunning after Shutdown = true")
	}

	// Subsequent Start returns ErrManagerClosed.
	err := mgr.Start(context.Background(), &domain.Bot{ID: 3, TenantID: 1, Name: "c", Platform: domain.PlatformTelegram})
	if !errors.Is(err, ErrManagerClosed) {
		t.Fatalf("post-shutdown Start err = %v want ErrManagerClosed", err)
	}

	// Shutdown is idempotent.
	mgr.Shutdown()
}

func TestManager_UnknownPlatformErr(t *testing.T) {
	mgr := NewManager(quietLogger())
	t.Cleanup(mgr.Shutdown)
	err := mgr.Start(context.Background(), &domain.Bot{
		ID: 1, TenantID: 1, Name: "x", Platform: "discord",
	})
	if !errors.Is(err, ErrUnknownPlatform) {
		t.Fatalf("err = %v want ErrUnknownPlatform", err)
	}
}

func TestManager_StartValidatesBot(t *testing.T) {
	factory := &fakeFactory{platform: domain.PlatformTelegram}
	mgr := NewManager(quietLogger(), factory)
	t.Cleanup(mgr.Shutdown)

	if err := mgr.Start(context.Background(), nil); err == nil {
		t.Fatal("expected error for nil bot")
	}
	if err := mgr.Start(context.Background(), &domain.Bot{ID: 0, Platform: domain.PlatformTelegram}); err == nil {
		t.Fatal("expected error for zero bot id")
	}
}

func TestManager_BuildErrorSurfaces(t *testing.T) {
	wantErr := errors.New("build boom")
	factory := &fakeFactory{
		platform: domain.PlatformTelegram,
		supplier: func(*domain.Bot) (Listener, error) { return nil, wantErr },
	}
	mgr := NewManager(quietLogger(), factory)
	t.Cleanup(mgr.Shutdown)

	err := mgr.Start(context.Background(), &domain.Bot{
		ID: 1, TenantID: 1, Name: "x", Platform: domain.PlatformTelegram,
	})
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("err = %v want wrapping %v", err, wantErr)
	}
	if mgr.IsRunning(1) {
		t.Fatal("IsRunning = true after Build failure")
	}
}

func TestManager_ListenerErrorCleansUp(t *testing.T) {
	l := newErrorListener(errors.New("permanent failure"))
	factory := &fakeFactory{
		platform: domain.PlatformTelegram,
		supplier: func(*domain.Bot) (Listener, error) { return l, nil },
	}
	mgr := NewManager(quietLogger(), factory)
	t.Cleanup(mgr.Shutdown)

	if err := mgr.Start(context.Background(), &domain.Bot{
		ID: 1, TenantID: 1, Name: "x", Platform: domain.PlatformTelegram,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitClosed(t, l.started, "listener should start")
	// Listener returns immediately with an error → Manager should
	// remove from active map.
	eventually(t, 2*time.Second, func() bool { return !mgr.IsRunning(1) })
}

func TestManager_PanicRecovered(t *testing.T) {
	l := newPanicListener()
	factory := &fakeFactory{
		platform: domain.PlatformTelegram,
		supplier: func(*domain.Bot) (Listener, error) { return l, nil },
	}
	mgr := NewManager(quietLogger(), factory)
	t.Cleanup(mgr.Shutdown)

	if err := mgr.Start(context.Background(), &domain.Bot{
		ID: 1, TenantID: 1, Name: "x", Platform: domain.PlatformTelegram,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitClosed(t, l.started, "listener should start")
	eventually(t, 2*time.Second, func() bool { return !mgr.IsRunning(1) })
}

func TestManager_StopUnknownBotIsNoop(t *testing.T) {
	mgr := NewManager(quietLogger())
	t.Cleanup(mgr.Shutdown)
	// Should not panic / block.
	mgr.Stop(9999)
}

func TestManager_ParentContextCancel(t *testing.T) {
	l := newFakeListener("x")
	factory := &fakeFactory{
		platform: domain.PlatformTelegram,
		supplier: func(*domain.Bot) (Listener, error) { return l, nil },
	}
	mgr := NewManager(quietLogger(), factory)
	t.Cleanup(mgr.Shutdown)

	ctx, cancel := context.WithCancel(context.Background())
	if err := mgr.Start(ctx, &domain.Bot{
		ID: 1, TenantID: 1, Name: "x", Platform: domain.PlatformTelegram,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitClosed(t, l.started, "listener should start")

	cancel() // parent ctx cancel cascades to listener
	eventually(t, 2*time.Second, func() bool { return !mgr.IsRunning(1) })
}
