package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/platform"
	"github.com/wendi/pulseguard/internal/platform/telegram"
)

// fakeTGServer is a scriptable Telegram backend used for the listener
// integration test. Per /getUpdates poll it returns the queued response,
// or a 200 OK empty result. Each /sendMessage invocation is recorded so
// the test can assert the listener replied with the right chat_id.
type fakeTGServer struct {
	*httptest.Server
	mu sync.Mutex

	updates  []string
	sent     []sentBody
	pollHits int32
}

type sentBody struct {
	ChatID float64
	Text   string
}

func newFakeTGServer() *fakeTGServer {
	f := &fakeTGServer{}
	f.Server = httptest.NewServer(http.HandlerFunc(f.handle))
	return f
}

func (f *fakeTGServer) handle(w http.ResponseWriter, r *http.Request) {
	if strings.Contains(r.URL.Path, "/getUpdates") {
		atomic.AddInt32(&f.pollHits, 1)
		f.mu.Lock()
		var body string
		if len(f.updates) > 0 {
			body = f.updates[0]
			f.updates = f.updates[1:]
		} else {
			body = `{"ok":true,"result":[]}`
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
		return
	}
	if strings.Contains(r.URL.Path, "/sendMessage") {
		raw, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		f.mu.Lock()
		f.sent = append(f.sent, sentBody{
			ChatID: floatOf(m["chat_id"]),
			Text:   stringOf(m["text"]),
		})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
		return
	}
	w.WriteHeader(404)
}

func (f *fakeTGServer) queue(body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, body)
}

func (f *fakeTGServer) sentSnapshot() []sentBody {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]sentBody, len(f.sent))
	copy(out, f.sent)
	return out
}

func floatOf(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}

func stringOf(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestRunBotListenerOnCreate verifies the end-to-end wire-up: register
// admin -> create bot via /api/v1/bots -> manager spawns a Telegram
// listener pointed at fakeTGServer -> queue an inbound /start update
// -> listener replies with the chat id via sendMessage. The bot token
// in this test is fabricated; it is NEVER the real user-supplied token.
func TestRunBotListenerOnCreate(t *testing.T) {
	tg := newFakeTGServer()
	defer tg.Close()
	// Queue an inbound /start update that the listener will pick up
	// on its first getUpdates call.
	tg.queue(`{"ok":true,"result":[{
		"update_id": 100,
		"message": {"chat": {"id": 555, "type": "private"}, "text": "/start"}
	}]}`)

	cfg := newTestConfig(t, "admin@example.com", "adminpass")
	cfg.Telegram.APIBase = tg.URL

	// Wire a real Telegram factory pointed at the fake server. We
	// leave the outbound Sender as a fakeSender — bot CRUD does not
	// trigger pushes.
	// The factory below uses telegram.NewFactory under the hood by
	// passing it through Overrides.BotListenerFactories. To avoid an
	// import cycle from this package, we use the runtime entrypoint
	// that already imports the telegram package.
	base, _, stop := startRuntimeWithBotListener(t, cfg, &fakeSender{}, tg.URL)
	defer stop()

	// Admin login.
	c := httpClient(t)
	loginBody := mustJSON(t, map[string]any{"email": "admin@example.com", "password": "adminpass"})
	resp := postJSON(t, c, base+"/api/v1/auth/login", loginBody, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin login: %d body=%s", resp.StatusCode, readErr(resp))
	}
	resp.Body.Close()
	csrf := jarCookie(t, c, base, "psg_csrf")

	// Create a bot — manager.Start runs after Insert returns.
	createBody := mustJSON(t, map[string]any{
		"name":      "listener-bot",
		"bot_token": "222222:fakeListenerToken",
		"platform":  "telegram",
	})
	botResp := postJSON(t, c, base+"/api/v1/bots", createBody, csrf)
	if botResp.StatusCode != http.StatusCreated {
		t.Fatalf("create bot: %d body=%s", botResp.StatusCode, readErr(botResp))
	}
	var botView map[string]any
	_ = json.NewDecoder(botResp.Body).Decode(&botView)
	botResp.Body.Close()
	if got, _ := botView["platform"].(string); got != "telegram" {
		t.Fatalf("platform = %q", got)
	}

	// Wait for the listener to consume the queued /start and reply via
	// sendMessage. The fake server records every call.
	eventually(t, 5*time.Second, func() bool {
		return len(tg.sentSnapshot()) >= 1
	})
	sent := tg.sentSnapshot()[0]
	if sent.ChatID != 555 {
		t.Fatalf("sendMessage chat_id = %v want 555", sent.ChatID)
	}
	if !strings.Contains(sent.Text, "555") {
		t.Fatalf("sendMessage text should contain chat id 555, got %q", sent.Text)
	}
}

// TestRunBotListenerSurvivesRestart verifies bots inserted before
// runtime start still get a listener — i.e. runtime calls ListAll()
// and spawns one Start per row. We insert a row, then start runtime,
// then push an update.
func TestRunBotListenerSurvivesRestart(t *testing.T) {
	tg := newFakeTGServer()
	defer tg.Close()
	tg.queue(`{"ok":true,"result":[{
		"update_id": 1,
		"message": {"chat": {"id": -777, "type": "supergroup"}, "text": "/chatid"}
	}]}`)

	cfg := newTestConfig(t, "admin@example.com", "adminpass")
	cfg.Telegram.APIBase = tg.URL

	// First boot: register admin, create bot, shut down.
	{
		base, _, stop := startRuntimeWithBotListener(t, cfg, &fakeSender{}, tg.URL)
		c := httpClient(t)
		loginBody := mustJSON(t, map[string]any{"email": "admin@example.com", "password": "adminpass"})
		resp := postJSON(t, c, base+"/api/v1/auth/login", loginBody, "")
		resp.Body.Close()
		csrf := jarCookie(t, c, base, "psg_csrf")

		createBody := mustJSON(t, map[string]any{
			"name":      "restart-bot",
			"bot_token": "333333:fakeRestart",
		})
		botResp := postJSON(t, c, base+"/api/v1/bots", createBody, csrf)
		if botResp.StatusCode != http.StatusCreated {
			t.Fatalf("create bot: %d body=%s", botResp.StatusCode, readErr(botResp))
		}
		botResp.Body.Close()
		// Stop runtime — listener for this bot is torn down.
		stop()
	}

	// Drain anything sent during the first boot.
	tg.mu.Lock()
	tg.sent = nil
	tg.mu.Unlock()
	// Re-queue an update for the second boot.
	tg.queue(`{"ok":true,"result":[{
		"update_id": 2,
		"message": {"chat": {"id": -777, "type": "supergroup"}, "text": "/chatid"}
	}]}`)

	// Second boot: runtime should call ListAll(), find the bot row,
	// and spawn the listener — which then replies to the queued update.
	_, _, stop2 := startRuntimeWithBotListener(t, cfg, &fakeSender{}, tg.URL)
	defer stop2()

	eventually(t, 5*time.Second, func() bool {
		return len(tg.sentSnapshot()) >= 1
	})
	sent := tg.sentSnapshot()[0]
	if sent.ChatID != -777 {
		t.Fatalf("chat_id = %v want -777", sent.ChatID)
	}
}

// startRuntimeWithBotListener is like startRuntime but injects a
// telegram listener factory whose APIBase points at the test's fake
// server. Kept inline (vs adding another Overrides knob to startRuntime)
// so the listener-integration tests have a single, explicit wiring point.
func startRuntimeWithBotListener(t *testing.T, cfg *config.Config, sender domain.Sender, tgAPIBase string) (string, *fakeSender, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	addrCh := make(chan net.Addr, 1)
	readyCh := make(chan struct{})
	doneCh := make(chan error, 1)

	fs, _ := sender.(*fakeSender)

	factory := telegram.NewFactory(telegram.FactoryOptions{
		APIBase: tgAPIBase,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	})

	go func() {
		doneCh <- RunWithDeps(ctx, cfg, testLogger(t), Overrides{
			Sender:               sender,
			ListenerCh:           addrCh,
			ReadyCh:              readyCh,
			BotListenerFactories: []platform.Factory{factory},
		})
	}()

	var addr net.Addr
	select {
	case addr = <-addrCh:
	case err := <-doneCh:
		cancel()
		t.Fatalf("runtime exited before binding: %v", err)
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for listener address")
	}
	select {
	case <-readyCh:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("timed out waiting for runtime readiness")
	}
	base := "http://" + addr.String()
	eventually(t, 3*time.Second, func() bool {
		resp, err := http.Get(base + "/healthz")
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	})
	stop := func() {
		cancel()
		select {
		case err := <-doneCh:
			if err != nil {
				t.Logf("runtime returned: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for runtime shutdown")
		}
	}
	return base, fs, stop
}
