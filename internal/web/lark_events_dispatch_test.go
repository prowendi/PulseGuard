package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/scripting"
)

// fakeLarkSender captures every Send call so tests can assert that
// the LB6 dispatch path produced a reply through deps.TG. It
// satisfies domain.Sender.
type fakeLarkSender struct {
	mu    sync.Mutex
	calls int32
	last  fakeLarkSendArgs
}

type fakeLarkSendArgs struct {
	botToken, chatID, parseMode, text string
}

func (f *fakeLarkSender) Send(_ context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	atomic.AddInt32(&f.calls, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.last = fakeLarkSendArgs{botToken, chatID, parseMode, text}
	return 0, nil
}

func (f *fakeLarkSender) callCount() int { return int(atomic.LoadInt32(&f.calls)) }

func (f *fakeLarkSender) lastArgs() fakeLarkSendArgs {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.last
}

// installFakeSender swaps deps.TG into a *fakeLarkSender and rebuilds
// the server with the new wiring. Returns the sender so tests can
// inspect it. We tear the existing httptest.Server down and start a
// fresh one — httptest forbids calling Start() on an already-started
// server, so a full re-create is the cleanest path.
func installFakeSender(t *testing.T, h *testHarness) *fakeLarkSender {
	t.Helper()
	fs := &fakeLarkSender{}
	h.deps.TG = fs
	h.deps.ScriptExec = &scripting.Executor{}
	h.server.Close()
	h.server = httptest.NewServer(NewServer(h.deps))
	h.cleanup = append(h.cleanup, func() { h.server.Close() })
	return fs
}

// seedLarkCommand inserts an enabled Starlark command bound to the
// supplied bot.
func seedLarkCommand(t *testing.T, h *testHarness, bot *domain.Bot, name, code string) *domain.Command {
	t.Helper()
	c := &domain.Command{
		TenantID: bot.TenantID,
		BotID:    bot.ID,
		Name:     name,
		Code:     code,
		Enabled:  true,
	}
	if err := h.deps.Commands.Insert(context.Background(), c); err != nil {
		t.Fatalf("insert command: %v", err)
	}
	return c
}

// makeLarkMessageEvent builds a v2 im.message.receive_v1 event body
// for the supplied bot.AppID + chat + text.
func makeLarkMessageEvent(appID, chatID, text string) []byte {
	content, _ := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	body, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{
			"event_id":   "evt-test",
			"event_type": "im.message.receive_v1",
			"app_id":     appID,
			"tenant_key": "tk",
			"token":      "vt",
		},
		"event": map[string]any{
			"message": map[string]any{
				"chat_id":      chatID,
				"open_chat_id": chatID,
				"message_type": "text",
				"content":      string(content),
			},
		},
	})
	return body
}

// TestLarkEvents_DispatchSlashCommandReplies wires the full path:
// signed event → command resolve → starlark execute → fake sender
// observes the reply.
func TestLarkEvents_DispatchSlashCommandReplies(t *testing.T) {
	h := newTestHarness(t)
	fs := installFakeSender(t, h)
	bot := seedLarkAppBot(t, h, "cli_dispatch", "enc-dispatch", true)
	seedLarkCommand(t, h, bot, "/echo", "def handle(args):\n    return \"echo: \" + \" \".join(args)\n")

	body := makeLarkMessageEvent("cli_dispatch", "oc_chat_abc", "/echo hello world")
	ts := "1"
	nonce := "n"
	sig := ComputeLarkSignature("enc-dispatch", ts, nonce, body)

	req := makeLarkReq(t, h, body, ts, nonce, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, drain(resp))
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["status"] != "dispatched" {
		t.Fatalf("status field = %v want dispatched", out["status"])
	}

	if fs.callCount() != 1 {
		t.Fatalf("sender calls = %d want 1", fs.callCount())
	}
	last := fs.lastArgs()
	if last.chatID != "oc_chat_abc" {
		t.Fatalf("reply chat_id = %q", last.chatID)
	}
	if !strings.Contains(last.text, "echo: hello world") {
		t.Fatalf("reply text = %q", last.text)
	}
	if !strings.HasPrefix(last.botToken, "lark-app://") {
		t.Fatalf("botToken = %q want lark-app://", last.botToken)
	}
}

// TestLarkEvents_NonSlashAcksWithoutReply confirms plain chat
// messages do NOT trigger Send.
func TestLarkEvents_NonSlashAcksWithoutReply(t *testing.T) {
	h := newTestHarness(t)
	fs := installFakeSender(t, h)
	seedLarkAppBot(t, h, "cli_plain", "k-plain", true)
	body := makeLarkMessageEvent("cli_plain", "oc_x", "hello, just chatting")
	ts := "1"
	nonce := "n"
	sig := ComputeLarkSignature("k-plain", ts, nonce, body)
	req := makeLarkReq(t, h, body, ts, nonce, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if fs.callCount() != 0 {
		t.Fatalf("sender invoked %d times for plain text", fs.callCount())
	}
}

// TestLarkEvents_UnknownCommandStaysSilent: /unknown command must
// NOT reach Send (matches Telegram listener's ErrDispatchSkip).
func TestLarkEvents_UnknownCommandStaysSilent(t *testing.T) {
	h := newTestHarness(t)
	fs := installFakeSender(t, h)
	seedLarkAppBot(t, h, "cli_unknown", "k-unknown", true)
	body := makeLarkMessageEvent("cli_unknown", "oc_x", "/notdefined")
	ts := "1"
	nonce := "n"
	sig := ComputeLarkSignature("k-unknown", ts, nonce, body)
	req := makeLarkReq(t, h, body, ts, nonce, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if fs.callCount() != 0 {
		t.Fatalf("sender invoked for unknown command (calls=%d)", fs.callCount())
	}
}

// TestLarkEvents_SubscriberUpsertedOnDispatch confirms LB6 records a
// (command, chat, platform=lark) subscriber row.
func TestLarkEvents_SubscriberUpsertedOnDispatch(t *testing.T) {
	h := newTestHarness(t)
	installFakeSender(t, h)
	bot := seedLarkAppBot(t, h, "cli_sub", "k", true)
	cmd := seedLarkCommand(t, h, bot, "/ping", "def handle(args):\n    return \"pong\"\n")

	body := makeLarkMessageEvent("cli_sub", "oc_sub_chat", "/ping")
	ts := "1"
	nonce := "n"
	sig := ComputeLarkSignature("k", ts, nonce, body)
	req := makeLarkReq(t, h, body, ts, nonce, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body=%s", resp.StatusCode, drain(resp))
	}
	subs, err := h.deps.Subscribers.ListByCommand(context.Background(), bot.TenantID, cmd.ID)
	if err != nil {
		t.Fatalf("ListByCommand: %v", err)
	}
	if len(subs) != 1 {
		t.Fatalf("subscribers = %d want 1", len(subs))
	}
	if subs[0].ChatID != "oc_sub_chat" {
		t.Fatalf("subscriber ChatID = %q", subs[0].ChatID)
	}
	if subs[0].Platform != domain.PlatformLark {
		t.Fatalf("subscriber Platform = %q want lark", subs[0].Platform)
	}
}

// TestParseSlashCommand pins the normalization rules in isolation so
// regressions don't slip past the integration tests above.
func TestParseSlashCommand(t *testing.T) {
	cases := []struct {
		in        string
		wantName  string
		wantArgs  []string
		wantOK    bool
	}{
		{"/echo hi", "echo", []string{"hi"}, true},
		{"/echo@mybot hi there", "echo", []string{"hi", "there"}, true},
		{"  /trim_leading args", "trim_leading", []string{"args"}, true},
		{"plain text", "", nil, false},
		{"/", "", nil, false},
		{" / ", "", nil, false},
		{"/cmd", "cmd", []string{}, true},
		{"/@only-bot-handle", "", nil, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.in, func(t *testing.T) {
			name, args, ok := parseSlashCommand(c.in)
			if ok != c.wantOK {
				t.Fatalf("ok = %v want %v", ok, c.wantOK)
			}
			if name != c.wantName {
				t.Fatalf("name = %q want %q", name, c.wantName)
			}
			if len(args) != len(c.wantArgs) {
				t.Fatalf("args len = %d want %d (%v)", len(args), len(c.wantArgs), args)
			}
			for i := range args {
				if args[i] != c.wantArgs[i] {
					t.Fatalf("args[%d] = %q want %q", i, args[i], c.wantArgs[i])
				}
			}
		})
	}
}

// TestParseInboundMessage_NonText returns false for unsupported
// message types (image, card, etc.) so the handler ACKs silently.
func TestParseInboundMessage_NonText(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{"event_type": "im.message.receive_v1"},
		"event": map[string]any{
			"message": map[string]any{
				"chat_id":      "oc_x",
				"message_type": "image",
				"content":      `{"image_key":"k"}`,
			},
		},
	})
	if _, ok := parseInboundMessage(body); ok {
		t.Fatal("image message must not parse as inbound text")
	}
}

// TestParseInboundMessage_BadInnerJSON: the doubly-encoded content
// must be valid JSON containing {"text": "..."}; bad inner JSON
// returns false (no panic, no spurious dispatch).
func TestParseInboundMessage_BadInnerJSON(t *testing.T) {
	body, _ := json.Marshal(map[string]any{
		"schema": "2.0",
		"header": map[string]any{"event_type": "im.message.receive_v1"},
		"event": map[string]any{
			"message": map[string]any{
				"chat_id":      "oc_x",
				"message_type": "text",
				"content":      `garbage{not json`,
			},
		},
	})
	if _, ok := parseInboundMessage(body); ok {
		t.Fatal("malformed inner JSON must not parse")
	}
}

// TestLarkEvents_NoExecutorStaysSilent: when the harness omits
// deps.ScriptExec the dispatch path bails before any Starlark, and
// the response stays a generic 200.
func TestLarkEvents_NoExecutorStaysSilent(t *testing.T) {
	h := newTestHarness(t)
	// Intentionally skip installFakeSender — we still want a fake to
	// observe the lack of a Send call. h.deps.ScriptExec stays nil so
	// the dispatch path must bail before invoking starlark.
	fs := &fakeLarkSender{}
	bot := seedLarkAppBot(t, h, "cli_noexec", "k-noexec", true)
	seedLarkCommand(t, h, bot, "/noop", "def handle(args):\n    return \"ok\"\n")

	// Tear down the existing httptest server and create a fresh one
	// with the modified deps.
	h.deps.TG = fs
	h.server.Close()
	h.server = httptest.NewServer(NewServer(h.deps))
	h.cleanup = append(h.cleanup, func() { h.server.Close() })

	body := makeLarkMessageEvent("cli_noexec", "oc_noexec", "/noop")
	ts := "1"
	nonce := "n"
	sig := ComputeLarkSignature("k-noexec", ts, nonce, body)
	req := makeLarkReq(t, h, body, ts, nonce, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if fs.callCount() != 0 {
		t.Fatalf("sender invoked despite missing executor")
	}
}

// TestLarkEvents_ScriptFailureFriendlyReply: an executor error
// surfaces as a Chinese-friendly fallback message to the user, NOT a
// silent drop.
func TestLarkEvents_ScriptFailureFriendlyReply(t *testing.T) {
	h := newTestHarness(t)
	fs := installFakeSender(t, h)
	bot := seedLarkAppBot(t, h, "cli_fail", "k-fail", true)
	seedLarkCommand(t, h, bot, "/boom", "def handle(args):\n    return undefined_symbol\n")

	body := makeLarkMessageEvent("cli_fail", "oc_fail", "/boom arg1")
	ts := "1"
	nonce := "n"
	sig := ComputeLarkSignature("k-fail", ts, nonce, body)
	req := makeLarkReq(t, h, body, ts, nonce, sig)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if fs.callCount() != 1 {
		t.Fatalf("sender calls = %d want 1 (friendly fallback)", fs.callCount())
	}
	last := fs.lastArgs()
	if !bytes.Contains([]byte(last.text), []byte("执行失败")) {
		t.Fatalf("fallback reply = %q (expected to mention 执行失败)", last.text)
	}
}
