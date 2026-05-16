package telegram

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeDispatcher is a scriptable CommandDispatcher used by the
// custom-command tests below. Each entry in `replies` maps a command
// name → DispatchOutput. Any entry in `errs` short-circuits with the
// supplied error before checking replies. Calls is the audit list of
// every input the listener handed us.
type fakeDispatcher struct {
	mu      sync.Mutex
	replies map[string]DispatchOutput
	errs    map[string]error
	calls   []DispatchInput
	skip    map[string]bool
}

func newFakeDispatcher() *fakeDispatcher {
	return &fakeDispatcher{
		replies: map[string]DispatchOutput{},
		errs:    map[string]error{},
		skip:    map[string]bool{},
	}
}

func (f *fakeDispatcher) Dispatch(ctx context.Context, in DispatchInput) (DispatchOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, in)
	if f.skip[in.Name] {
		return DispatchOutput{}, ErrDispatchSkip
	}
	if e, ok := f.errs[in.Name]; ok {
		return DispatchOutput{}, e
	}
	if out, ok := f.replies[in.Name]; ok {
		return out, nil
	}
	return DispatchOutput{}, ErrDispatchSkip
}

func (f *fakeDispatcher) Calls() []DispatchInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]DispatchInput, len(f.calls))
	copy(out, f.calls)
	return out
}

// startListenerWithDispatcher mirrors startListener but injects a fake
// dispatcher so the per-tenant custom-command path can be exercised.
func startListenerWithDispatcher(t *testing.T, srv *fakeTG, disp CommandDispatcher) (func(), chan error) {
	t.Helper()
	l, err := New(botFixture(), Options{
		APIBase:    srv.URL,
		HTTP:       srv.Client(),
		Logger:     quietLogger(),
		Dispatcher: disp,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	return cancel, errCh
}

func TestListener_CustomCommandDispatched(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":500},"text":"/查询 alpha 7"}
	}]}`)

	disp := newFakeDispatcher()
	disp.replies["查询"] = DispatchOutput{Text: "结果: alpha=7"}

	cancel, errCh := startListenerWithDispatcher(t, srv, disp)
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(disp.Calls()) >= 1 })
	calls := disp.Calls()
	if len(calls) == 0 {
		t.Fatal("dispatcher was not called")
	}
	in := calls[0]
	if in.Name != "查询" {
		t.Fatalf("name = %q, want 查询", in.Name)
	}
	if len(in.Args) != 2 || in.Args[0] != "alpha" || in.Args[1] != "7" {
		t.Fatalf("args = %v, want [alpha 7]", in.Args)
	}
	if in.ChatID != 500 {
		t.Fatalf("chat = %d, want 500", in.ChatID)
	}

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	sent := srv.sentSnapshot()[0]
	if !strings.Contains(sent.Text, "结果: alpha=7") {
		t.Fatalf("sent text missing dispatcher reply: %q", sent.Text)
	}
}

func TestListener_CustomCommandSkipsWhenUnknown(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":501},"text":"/missing"}
	}]}`)
	srv.queueUpdates(`{"ok":true,"result":[]}`)

	disp := newFakeDispatcher()
	disp.skip["missing"] = true

	cancel, errCh := startListenerWithDispatcher(t, srv, disp)
	defer func() { cancel(); <-errCh }()

	// Give the loop room to process and then idle once.
	eventually(t, 3*time.Second, func() bool { return len(disp.Calls()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := srv.sentSnapshot(); len(got) != 0 {
		t.Fatalf("expected silence for unknown command, got %d replies (%+v)", len(got), got)
	}
}

func TestListener_CustomCommandTimeoutFriendlyMessage(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":502},"text":"/slow"}
	}]}`)

	disp := newFakeDispatcher()
	disp.errs["slow"] = ErrDispatchTimeout

	cancel, errCh := startListenerWithDispatcher(t, srv, disp)
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	if got := srv.sentSnapshot()[0].Text; !strings.Contains(got, "超时") {
		t.Fatalf("expected friendly timeout message, got %q", got)
	}
}

func TestListener_CustomCommandUnsafeHostFriendlyMessage(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":503},"text":"/ssrf"}
	}]}`)

	disp := newFakeDispatcher()
	disp.errs["ssrf"] = ErrDispatchUnsafeHost

	cancel, errCh := startListenerWithDispatcher(t, srv, disp)
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	if got := srv.sentSnapshot()[0].Text; !strings.Contains(got, "不允许") {
		t.Fatalf("expected friendly SSRF message, got %q", got)
	}
}

func TestListener_CustomCommandGenericErrorFriendlyMessage(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":504},"text":"/broken"}
	}]}`)

	disp := newFakeDispatcher()
	disp.errs["broken"] = errors.New("KABOOM internal stack trace blah blah")

	cancel, errCh := startListenerWithDispatcher(t, srv, disp)
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	got := srv.sentSnapshot()[0].Text
	if !strings.Contains(got, "失败") {
		t.Fatalf("expected friendly generic error, got %q", got)
	}
	if strings.Contains(got, "KABOOM") {
		t.Fatalf("internal error leaked to chat: %q", got)
	}
}

func TestListener_CustomCommandBotnameSuffixStripped(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":505},"text":"/查询@test_bot 1 2"}
	}]}`)

	disp := newFakeDispatcher()
	disp.replies["查询"] = DispatchOutput{Text: "ok"}

	cancel, errCh := startListenerWithDispatcher(t, srv, disp)
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(disp.Calls()) >= 1 })
	if disp.Calls()[0].Name != "查询" {
		t.Fatalf("name = %q want 查询 (suffix not stripped)", disp.Calls()[0].Name)
	}
}

func TestListener_CustomCommandWithoutDispatcherSilent(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":506},"text":"/anything"}
	}]}`)
	srv.queueUpdates(`{"ok":true,"result":[]}`)

	// Don't pass a Dispatcher — listener should stay silent.
	cancel, errCh := startListener(t, srv, botFixture())
	defer func() { cancel(); <-errCh }()

	time.Sleep(150 * time.Millisecond)
	if got := srv.sentSnapshot(); len(got) != 0 {
		t.Fatalf("expected silence without dispatcher, got %d (%+v)", len(got), got)
	}
}
