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
