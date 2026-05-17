package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
)

// fakeTG is a scriptable Telegram backend. Each call to /getUpdates
// returns the next pre-queued response; /sendMessage records calls.
// All accessors are mutex-guarded so the long-poll goroutine and the
// test goroutine can both interact safely.
type fakeTG struct {
	*httptest.Server
	mu sync.Mutex

	// queued response bodies for /getUpdates. Each entry is a complete
	// JSON envelope returned in order. When the queue is empty we
	// reply with an empty result array (ok:true, []) — simulating
	// a normal idle long-poll.
	updates []fakeResp

	// sent captures every /sendMessage call.
	sent []sentMsg

	// statusForSendMessage forces a non-200 reply to sendMessage. 0
	// means 200.
	statusForSendMessage int

	getUpdatesCalls int32

	// setMyCommandsCalls captures every setMyCommands invocation so
	// tests can assert that listener startup publishes the slash menu
	// exactly once. Body is the raw POST payload as the listener sent
	// it; tests grep for "commands" / individual command names.
	setMyCommandsCalls []setMyCmdCall
}

type setMyCmdCall struct {
	Body string
}

type fakeResp struct {
	status int
	body   string
}

type sentMsg struct {
	ChatID float64
	Text   string
}

func newFakeTG() *fakeTG {
	f := &fakeTG{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeTG) handle(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/getUpdates") {
		atomic.AddInt32(&f.getUpdatesCalls, 1)
		f.mu.Lock()
		var resp fakeResp
		if len(f.updates) > 0 {
			resp = f.updates[0]
			f.updates = f.updates[1:]
		} else {
			resp = fakeResp{status: 200, body: `{"ok":true,"result":[]}`}
		}
		f.mu.Unlock()
		if resp.status == 0 {
			resp.status = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = io.WriteString(w, resp.body)
		return
	}
	if strings.Contains(r.URL.Path, "/sendMessage") {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		f.mu.Lock()
		f.sent = append(f.sent, sentMsg{
			ChatID: castFloat(m["chat_id"]),
			Text:   castString(m["text"]),
		})
		st := f.statusForSendMessage
		f.mu.Unlock()
		if st == 0 {
			st = 200
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		if st == 200 {
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
			return
		}
		_, _ = io.WriteString(w, `{"ok":false,"error_code":`+fmt.Sprint(st)+`,"description":"err"}`)
		return
	}
	if strings.Contains(r.URL.Path, "/setMyCommands") {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.setMyCommandsCalls = append(f.setMyCommandsCalls, setMyCmdCall{Body: string(body)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"ok":true,"result":true}`)
		return
	}
	w.WriteHeader(404)
}

func (f *fakeTG) queueUpdates(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, fakeResp{status: 200, body: body})
}

func (f *fakeTG) queueResp(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, fakeResp{status: status, body: body})
}

func (f *fakeTG) sentSnapshot() []sentMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentMsg, len(f.sent))
	copy(out, f.sent)
	return out
}

// setMyCommandsSnapshot returns a copy of every setMyCommands payload
// the listener POSTed so tests can assert (a) startup publishes the
// menu exactly once and (b) the JSON body contains the expected
// command names.
func (f *fakeTG) setMyCommandsSnapshot() []setMyCmdCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]setMyCmdCall, len(f.setMyCommandsCalls))
	copy(out, f.setMyCommandsCalls)
	return out
}

func castFloat(v any) float64 {
	if v == nil {
		return 0
	}
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func castString(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func botFixture() *domain.Bot {
	// The bot_id "111111" is what new_chat_members entries must match
	// for the group-join self-detection.
	return &domain.Bot{
		ID:       42,
		TenantID: 7,
		Name:     "test-bot",
		Platform: domain.PlatformTelegram,
		BotToken: "111111:AAAAtestSecret",
	}
}

func startListener(t *testing.T, srv *fakeTG, bot *domain.Bot) (context.CancelFunc, chan error) {
	t.Helper()
	l, err := New(bot, Options{APIBase: srv.URL, HTTP: srv.Client(), Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	return cancel, errCh
}

func eventually(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not satisfied")
}

func TestListener_ParseBotIDValid(t *testing.T) {
	id, err := parseBotID("999:secret")
	if err != nil {
		t.Fatalf("parseBotID: %v", err)
	}
	if id != 999 {
		t.Fatalf("id = %d want 999", id)
	}
}

func TestListener_ParseBotIDInvalid(t *testing.T) {
	cases := []string{"", "no-colon", ":secret", "abc:def", "0:secret", "-1:secret"}
	for _, c := range cases {
		if _, err := parseBotID(c); err == nil {
			t.Fatalf("parseBotID(%q) returned nil, want err", c)
		}
	}
}

func TestListener_New_RejectsNilOrEmptyToken(t *testing.T) {
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("expected error for nil bot")
	}
	if _, err := New(&domain.Bot{}, Options{}); err == nil {
		t.Fatal("expected error for empty token")
	}
	if _, err := New(&domain.Bot{BotToken: "bad"}, Options{}); err == nil {
		t.Fatal("expected error for malformed token")
	}
}

func TestListener_StartCommandRepliesWithChatID(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1001,
		"message": {"chat": {"id": -100123, "type": "supergroup"}, "text": "/start"}
	}]}`)

	cancel, errCh := startListener(t, srv, botFixture())
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool {
		return len(srv.sentSnapshot()) >= 1
	})
	sent := srv.sentSnapshot()[0]
	if sent.ChatID != -100123 {
		t.Fatalf("chat_id = %v want -100123", sent.ChatID)
	}
	if !strings.Contains(sent.Text, "-100123") {
		t.Fatalf("reply missing chat id, got %q", sent.Text)
	}
	if !strings.Contains(sent.Text, "PulseGuard") {
		t.Fatalf("reply missing banner: %q", sent.Text)
	}
}

func TestListener_ChatIDCommandReplies(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":42}, "text":"/chatid"}
	}]}`)

	cancel, errCh := startListener(t, srv, botFixture())
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
}

func TestListener_StartCommandWithBotnameSuffix(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	// Group messages append @botname to commands.
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":50}, "text":"/start@test_bot extra"}
	}]}`)

	cancel, errCh := startListener(t, srv, botFixture())
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
}

func TestListener_NewChatMembersSelfTriggersReply(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	// new_chat_members containing our own bot id (111111) → reply.
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 5, "message": {
			"chat":{"id":-200,"type":"group"},
			"new_chat_members":[{"id":111111,"is_bot":true},{"id":1,"is_bot":false}]
		}
	}]}`)

	cancel, errCh := startListener(t, srv, botFixture())
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	if got := srv.sentSnapshot()[0].ChatID; got != -200 {
		t.Fatalf("chat_id = %v want -200", got)
	}
}

func TestListener_NewChatMembersWithoutSelfIgnored(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	// new_chat_members does NOT include our bot id; we must stay silent.
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 5, "message": {
			"chat":{"id":-200,"type":"group"},
			"new_chat_members":[{"id":1,"is_bot":false},{"id":999,"is_bot":true}]
		}
	}]}`)
	// Then a normal idle round so the loop has another tick.
	srv.queueUpdates(`{"ok":true,"result":[]}`)

	cancel, errCh := startListener(t, srv, botFixture())
	defer cancel()

	// Wait until at least 2 getUpdates calls so we know the message
	// was processed (no reply expected).
	eventually(t, 3*time.Second, func() bool { return atomic.LoadInt32(&srv.getUpdatesCalls) >= 2 })
	if got := srv.sentSnapshot(); len(got) != 0 {
		t.Fatalf("expected no replies, got %d (%+v)", len(got), got)
	}
	cancel()
	<-errCh
}

func TestListener_OffsetAdvances(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	// Two updates, processed in order; the second getUpdates call MUST
	// carry offset >= 11 (10+1).
	srv.queueUpdates(`{"ok":true,"result":[{"update_id":10,"message":{"chat":{"id":1},"text":"/start"}}]}`)
	srv.queueUpdates(`{"ok":true,"result":[{"update_id":15,"message":{"chat":{"id":2},"text":"/start"}}]}`)

	type captured struct{ offset string }
	captures := make([]captured, 0, 4)
	var capMu sync.Mutex

	// Replace handler with a snooping wrapper.
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/getUpdates") {
			capMu.Lock()
			captures = append(captures, captured{offset: r.URL.Query().Get("offset")})
			capMu.Unlock()
		}
		// Proxy through to the fake.
		req, _ := http.NewRequest(r.Method, srv.URL+r.URL.String(), r.Body)
		req.Header = r.Header
		resp, err := srv.Client().Do(req)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
	defer hook.Close()

	bot := botFixture()
	l, err := New(bot, Options{APIBase: hook.URL, HTTP: hook.Client(), Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()

	eventually(t, 3*time.Second, func() bool {
		capMu.Lock()
		defer capMu.Unlock()
		return len(captures) >= 3
	})
	cancel()
	<-errCh

	capMu.Lock()
	defer capMu.Unlock()
	if len(captures) < 3 {
		t.Fatalf("captures len = %d", len(captures))
	}
	if captures[0].offset != "0" {
		t.Fatalf("first offset = %q want 0", captures[0].offset)
	}
	if captures[1].offset != "11" {
		t.Fatalf("second offset = %q want 11 (10+1)", captures[1].offset)
	}
	// Third call advances past 15.
	if captures[2].offset != "16" {
		t.Fatalf("third offset = %q want 16 (15+1)", captures[2].offset)
	}
}

func TestListener_401ReturnsErrTokenInvalid(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueResp(401, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)

	bot := botFixture()
	l, err := New(bot, Options{APIBase: srv.URL, HTTP: srv.Client(), Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrTokenInvalid) {
			t.Fatalf("Run returned %v want ErrTokenInvalid", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return on 401")
	}
}

func TestListener_CtxCancelReturnsNil(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	bot := botFixture()
	l, err := New(bot, Options{APIBase: srv.URL, HTTP: srv.Client(), Logger: quietLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()

	// Allow the first getUpdates to be issued, then cancel.
	eventually(t, 2*time.Second, func() bool { return atomic.LoadInt32(&srv.getUpdatesCalls) >= 1 })
	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("Run returned %v want nil on ctx cancel", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return on ctx cancel")
	}
}

func TestListener_429Backoff(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueResp(429, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`)
	// Subsequent call returns a /start so we can confirm the listener resumed.
	srv.queueUpdates(`{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":7},"text":"/start"}}]}`)

	cancel, errCh := startListener(t, srv, botFixture())
	defer func() { cancel(); <-errCh }()

	eventually(t, 5*time.Second, func() bool {
		return len(srv.sentSnapshot()) >= 1
	})
	if got := srv.sentSnapshot()[0].ChatID; got != 7 {
		t.Fatalf("chat_id = %v want 7", got)
	}
}

func TestListener_NonFatalErrorRetries(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueResp(500, `{"ok":false,"error_code":500,"description":"server"}`)
	srv.queueUpdates(`{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":9},"text":"/start"}}]}`)

	cancel, errCh := startListener(t, srv, botFixture())
	defer func() { cancel(); <-errCh }()

	eventually(t, 10*time.Second, func() bool {
		return len(srv.sentSnapshot()) >= 1
	})
}

func TestListener_EmptyTextIgnored(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{"update_id":1,"message":{"chat":{"id":7}}}]}`)
	srv.queueUpdates(`{"ok":true,"result":[]}`)

	cancel, errCh := startListener(t, srv, botFixture())
	defer cancel()
	eventually(t, 3*time.Second, func() bool {
		return atomic.LoadInt32(&srv.getUpdatesCalls) >= 2
	})
	if got := srv.sentSnapshot(); len(got) != 0 {
		t.Fatalf("expected no replies for empty text, got %d", len(got))
	}
	cancel()
	<-errCh
}

func TestFactoryBuilds(t *testing.T) {
	f := NewFactory(FactoryOptions{APIBase: "http://example.invalid", Logger: quietLogger()})
	if f.Platform() != domain.PlatformTelegram {
		t.Fatalf("platform = %q", f.Platform())
	}
	l, err := f.Build(botFixture())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if l == nil {
		t.Fatal("Build returned nil listener")
	}
}

func TestFactoryRejectsBadBot(t *testing.T) {
	f := NewFactory(FactoryOptions{})
	if _, err := f.Build(&domain.Bot{ID: 1, BotToken: "bad-no-colon"}); err == nil {
		t.Fatal("expected error from Build with malformed token")
	}
}

// fakeCatalog scriptable telegram.CommandCatalog used to assert that
// listener startup publishes the slash menu via setMyCommands. Calls
// is recorded so we can verify the catalog is consulted exactly once.
type fakeCatalog struct {
	mu    sync.Mutex
	rows  []CommandSummary
	calls int
}

func (c *fakeCatalog) ListByBot(_ context.Context, _ int64) ([]CommandSummary, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	out := make([]CommandSummary, len(c.rows))
	copy(out, c.rows)
	return out, nil
}

func (c *fakeCatalog) Calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestListener_SetMyCommandsPublishedOnStartup(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	cat := &fakeCatalog{rows: []CommandSummary{
		{Name: "/echo", Description: "echo the args"},
		{Name: "查询", Description: "查询订单"},
		{Name: "/blank", Description: ""}, // exercises fallback
	}}
	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Catalog: cat,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool {
		return len(srv.setMyCommandsSnapshot()) >= 1
	})
	calls := srv.setMyCommandsSnapshot()
	if got := len(calls); got != 1 {
		t.Fatalf("setMyCommands call count = %d, want exactly 1 on startup", got)
	}
	if cat.Calls() != 1 {
		t.Fatalf("catalog.ListByBot called %d times, want 1", cat.Calls())
	}
	body := calls[0].Body
	// JSON body must carry "commands" and each command name WITHOUT
	// leading slash. Empty descriptions fall back to "(no description)".
	for _, frag := range []string{`"commands"`, `"echo"`, `"查询"`, `"blank"`, `"(no description)"`} {
		if !strings.Contains(body, frag) {
			t.Fatalf("setMyCommands body missing fragment %q; got: %s", frag, body)
		}
	}
	if strings.Contains(body, `"/echo"`) {
		t.Fatalf("command name leaked leading slash: %s", body)
	}
}

func TestListener_SetMyCommandsSkippedWhenNoCatalog(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	// No catalog wired — listener must NOT call setMyCommands.
	cancel, errCh := startListener(t, srv, botFixture())
	defer func() { cancel(); <-errCh }()

	// Wait for at least one getUpdates round so the loop has finished
	// startup; setMyCommands either happens before or never.
	eventually(t, 3*time.Second, func() bool { return atomic.LoadInt32(&srv.getUpdatesCalls) >= 1 })
	if got := len(srv.setMyCommandsSnapshot()); got != 0 {
		t.Fatalf("setMyCommands called %d times without a catalog, want 0", got)
	}
}

// fakeRemover is a scriptable telegram.SubscriberRemover used by the
// /unsubscribe tests. notFound[name]=true makes DeleteByChatAndCommand
// return domain.ErrNotFound for that command name; otherwise the call
// is recorded and returns nil.
type fakeRemover struct {
	mu       sync.Mutex
	notFound map[string]bool
	calls    []removeCall
}

type removeCall struct {
	BotID   int64
	ChatID  string
	Command string
}

func newFakeRemover() *fakeRemover {
	return &fakeRemover{notFound: map[string]bool{}}
}

func (r *fakeRemover) DeleteByChatAndCommand(_ context.Context, botID int64, chatID, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, removeCall{BotID: botID, ChatID: chatID, Command: name})
	if r.notFound[name] {
		return domain.ErrNotFound
	}
	return nil
}

func (r *fakeRemover) Calls() []removeCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]removeCall, len(r.calls))
	copy(out, r.calls)
	return out
}

func TestListener_CommandsBuiltinListsCatalog(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":700},"text":"/commands"}
	}]}`)

	cat := &fakeCatalog{rows: []CommandSummary{
		{Name: "/echo", Description: "echo back args"},
		{Name: "查询", Description: ""},
	}}
	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Catalog: cat,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	reply := srv.sentSnapshot()[0]
	if reply.ChatID != 700 {
		t.Fatalf("reply chat_id = %v, want 700", reply.ChatID)
	}
	for _, frag := range []string{"可用命令", "/echo", "echo back args", "/查询"} {
		if !strings.Contains(reply.Text, frag) {
			t.Fatalf("/commands reply missing %q: %q", frag, reply.Text)
		}
	}
}

func TestListener_CommandsBuiltinEmptyCatalog(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":701},"text":"/commands"}
	}]}`)

	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Catalog: &fakeCatalog{rows: nil},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	if got := srv.sentSnapshot()[0].Text; !strings.Contains(got, "暂无") {
		t.Fatalf("expected empty-catalog hint, got %q", got)
	}
}

func TestListener_UnsubscribeBuiltinHappyPath(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":800},"text":"/unsubscribe echo"}
	}]}`)

	rem := newFakeRemover()
	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Remover: rem,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(rem.Calls()) >= 1 })
	call := rem.Calls()[0]
	// BotID is the DB bot.ID (42 in botFixture), not the Telegram
	// token prefix 111111. ChatID is stringified.
	if call.BotID != 42 {
		t.Fatalf("BotID = %d, want 42 (DB bot.ID)", call.BotID)
	}
	if call.ChatID != "800" {
		t.Fatalf("ChatID = %q, want \"800\"", call.ChatID)
	}
	if call.Command != "echo" {
		t.Fatalf("Command = %q, want echo", call.Command)
	}
	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	if got := srv.sentSnapshot()[0].Text; !strings.Contains(got, "已取消订阅") {
		t.Fatalf("expected confirmation, got %q", got)
	}
}

func TestListener_UnsubscribeBuiltinNotFound(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":801},"text":"/unsubscribe ghost"}
	}]}`)

	rem := newFakeRemover()
	rem.notFound["ghost"] = true
	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Remover: rem,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	if got := srv.sentSnapshot()[0].Text; !strings.Contains(got, "未订阅") {
		t.Fatalf("expected '未订阅' reply, got %q", got)
	}
}

func TestListener_UnsubscribeBuiltinUsageHint(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":802},"text":"/unsubscribe"}
	}]}`)

	rem := newFakeRemover()
	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Remover: rem,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	if got := srv.sentSnapshot()[0].Text; !strings.Contains(got, "用法") {
		t.Fatalf("expected usage hint, got %q", got)
	}
	// Remover must NOT have been called with an empty name.
	if got := len(rem.Calls()); got != 0 {
		t.Fatalf("remover called %d times for arg-less /unsubscribe, want 0", got)
	}
}

// fakeAcker is a scriptable telegram.AlertAcker used by /ack tests.
// dup=true makes Insert return ErrAckAlreadyExists; otherwise the call
// is recorded and returns nil.
type fakeAcker struct {
	mu    sync.Mutex
	dup   bool
	calls []AckInput
}

func newFakeAcker() *fakeAcker { return &fakeAcker{} }

func (a *fakeAcker) Insert(_ context.Context, in AckInput) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls = append(a.calls, in)
	if a.dup {
		return ErrAckAlreadyExists
	}
	return nil
}

func (a *fakeAcker) Calls() []AckInput {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]AckInput, len(a.calls))
	copy(out, a.calls)
	return out
}

func TestListener_AckBuiltinHappyPath(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	// Message carries a from.username so the audit row uses @alice.
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {
			"chat":{"id":900},
			"from":{"id":555,"is_bot":false,"username":"alice"},
			"text":"/ack abc123"
		}
	}]}`)

	ack := newFakeAcker()
	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Acker:   ack,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(ack.Calls()) >= 1 })
	call := ack.Calls()[0]
	if call.BotID != 42 {
		t.Fatalf("BotID = %d, want 42 (DB bot.ID)", call.BotID)
	}
	if call.Fingerprint != "abc123" {
		t.Fatalf("Fingerprint = %q, want abc123", call.Fingerprint)
	}
	if call.ChatID != "900" {
		t.Fatalf("ChatID = %q, want \"900\"", call.ChatID)
	}
	if call.AckedBy != "@alice" {
		t.Fatalf("AckedBy = %q, want @alice", call.AckedBy)
	}
	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	got := srv.sentSnapshot()[0].Text
	if !strings.Contains(got, "已 ACK") || !strings.Contains(got, "abc123") || !strings.Contains(got, "@alice") {
		t.Fatalf("unexpected reply: %q", got)
	}
}

func TestListener_AckBuiltinDuplicateFriendly(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {
			"chat":{"id":901}, "text":"/ack dup-fp"
		}
	}]}`)

	ack := newFakeAcker()
	ack.dup = true
	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Acker:   ack,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	got := srv.sentSnapshot()[0].Text
	if !strings.Contains(got, "已记录") {
		t.Fatalf("expected duplicate-friendly reply, got %q", got)
	}
}

func TestListener_AckBuiltinUsageHint(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":902}, "text":"/ack"}
	}]}`)

	ack := newFakeAcker()
	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Acker:   ack,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	if got := srv.sentSnapshot()[0].Text; !strings.Contains(got, "用法") {
		t.Fatalf("expected usage hint, got %q", got)
	}
	if got := len(ack.Calls()); got != 0 {
		t.Fatalf("acker called %d times for arg-less /ack, want 0", got)
	}
}

func TestListener_AckBuiltinChatFallback(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	// No from field → AckedBy falls back to "chat:<chat_id>".
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":903}, "text":"/ack fp-x"}
	}]}`)

	ack := newFakeAcker()
	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Acker:   ack,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(ack.Calls()) >= 1 })
	if got := ack.Calls()[0].AckedBy; got != "chat:903" {
		t.Fatalf("AckedBy = %q, want chat:903", got)
	}
}

// TestListener_HealthHookFiresOnUpdate verifies the listener bumps
// the HealthHook callbacks for the V6-2 in-memory health panel:
// non-empty getUpdates batches fire OnUpdate, dispatched commands
// fire OnDispatch. Built-in /start does NOT fire OnDispatch.
func TestListener_HealthHookFiresOnUpdate(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":42},"text":"/查询"}
	}]}`)

	var (
		updates   int32
		dispatch  int32
	)
	hook := HealthHook{
		OnUpdate:   func(_ int64) { atomic.AddInt32(&updates, 1) },
		OnDispatch: func(_ int64) { atomic.AddInt32(&dispatch, 1) },
	}
	disp := newFakeDispatcher()
	disp.replies["查询"] = DispatchOutput{Text: "ok"}

	l, err := New(botFixture(), Options{
		APIBase:    srv.URL,
		HTTP:       srv.Client(),
		Logger:     quietLogger(),
		Dispatcher: disp,
		Health:     hook,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool {
		return atomic.LoadInt32(&updates) >= 1 && atomic.LoadInt32(&dispatch) >= 1
	})
	if got := atomic.LoadInt32(&updates); got < 1 {
		t.Fatalf("OnUpdate fired %d times, want >=1", got)
	}
	if got := atomic.LoadInt32(&dispatch); got != 1 {
		t.Fatalf("OnDispatch fired %d times, want 1", got)
	}
}

// TestListener_HealthHookOnBuiltinSkipsDispatch verifies built-in
// /start/chatid/commands do NOT increment OnDispatch — those are
// onboarding/management, not "the bot is doing work" signals.
func TestListener_HealthHookOnBuiltinSkipsDispatch(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 1, "message": {"chat":{"id":42},"text":"/start"}
	}]}`)

	var dispatch int32
	hook := HealthHook{
		OnDispatch: func(_ int64) { atomic.AddInt32(&dispatch, 1) },
	}
	l, err := New(botFixture(), Options{
		APIBase: srv.URL,
		HTTP:    srv.Client(),
		Logger:  quietLogger(),
		Health:  hook,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- l.Run(ctx) }()
	defer func() { cancel(); <-errCh }()

	eventually(t, 3*time.Second, func() bool { return len(srv.sentSnapshot()) >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&dispatch); got != 0 {
		t.Fatalf("OnDispatch fired %d times for /start, want 0", got)
	}
}
