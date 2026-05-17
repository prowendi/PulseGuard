package platform

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prowendi/PulseGuard/internal/domain"
)

// TestManager_Health_RecordsUpdateDispatchError exercises the V6-2
// in-memory health panel. The Manager exposes RecordUpdate /
// RecordDispatch / RecordError which the listener (and tests) invoke
// directly. Health(botID) returns a by-value snapshot the web layer
// renders into the bots-page status column.
func TestManager_Health_RecordsUpdateDispatchError(t *testing.T) {
	frozen := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	mgr := NewManager(quietLogger())
	mgr.now = func() time.Time { return frozen }
	t.Cleanup(mgr.Shutdown)

	// No signal yet → zero value.
	if h := mgr.Health(42); !h.LastSeenAt.IsZero() || h.UpdatesReceived != 0 {
		t.Fatalf("initial Health = %+v, want zero value", h)
	}

	mgr.RecordUpdate(42)
	if h := mgr.Health(42); h.UpdatesReceived != 1 || !h.LastSeenAt.Equal(frozen) {
		t.Fatalf("after RecordUpdate Health = %+v", h)
	}

	mgr.RecordDispatch(42)
	if h := mgr.Health(42); h.CommandsDispatched != 1 || h.UpdatesReceived != 1 {
		t.Fatalf("after RecordDispatch counters wrong: %+v", h)
	}

	mgr.RecordError(42, "boom")
	h := mgr.Health(42)
	if h.LastError != "boom" || !h.LastErrorAt.Equal(frozen) {
		t.Fatalf("after RecordError = %+v, want LastError=boom", h)
	}

	// Snapshot returns a map — must contain the same bot id.
	snap := mgr.HealthSnapshot()
	if _, ok := snap[42]; !ok {
		t.Fatal("HealthSnapshot missing bot id 42")
	}
}

// TestManager_Health_RecordErrorTruncates verifies the recorder caps
// long error messages so a tooltip cannot blow up the bots page.
func TestManager_Health_RecordErrorTruncates(t *testing.T) {
	mgr := NewManager(quietLogger())
	t.Cleanup(mgr.Shutdown)

	long := make([]byte, 500)
	for i := range long {
		long[i] = 'x'
	}
	mgr.RecordError(99, string(long))
	if got := mgr.Health(99); len(got.LastError) != 200 {
		t.Fatalf("LastError len = %d, want 200", len(got.LastError))
	}
}

// TestManager_Health_NilCallbackSafe ensures that a zero-value
// recorder is safe to call repeatedly on a freshly-built Manager.
func TestManager_Health_ZeroBotIDIgnored(t *testing.T) {
	mgr := NewManager(quietLogger())
	t.Cleanup(mgr.Shutdown)
	// botID 0 is not a real bot — ignore (so a poorly-wired listener
	// cannot pollute the health map with a phantom row).
	mgr.RecordUpdate(0)
	mgr.RecordDispatch(0)
	mgr.RecordError(0, "anything")
	if got := mgr.HealthSnapshot(); len(got) != 0 {
		t.Fatalf("HealthSnapshot = %v, want empty", got)
	}
}

// Compile-time sanity: ensure the BotHealth zero value remains usable.
var _ BotHealth = BotHealth{}

// Compile-time guards that the test imports are not stripped.
var (
	_ = errors.New
	_ = context.Background
	_ = domain.PlatformTelegram
)
