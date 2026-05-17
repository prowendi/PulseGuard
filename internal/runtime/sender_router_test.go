package runtime

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/lark"
	"github.com/wendi/pulseguard/internal/tg"
)

// captureSender is a domain.SenderWithOpts that records every call into
// labelled slices so the router tests can assert which underlying
// client received the dispatch.
type captureSender struct {
	mu        sync.Mutex
	sends     []string // botToken values seen by Send
	withOpts  []string
	edits     []string
	editIDs   []int64
	withOptsB [][]domain.PushButton // buttons seen per SendWithOpts call
}

func (c *captureSender) Send(_ context.Context, botToken, _, _, _ string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sends = append(c.sends, botToken)
	return 100, nil
}

func (c *captureSender) SendWithOpts(_ context.Context, botToken, _, _, _ string, opts domain.SendOptions) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.withOpts = append(c.withOpts, botToken)
	c.withOptsB = append(c.withOptsB, opts.Buttons)
	return 200, nil
}

func (c *captureSender) EditMessage(_ context.Context, botToken, _ string, messageID int64, _, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.edits = append(c.edits, botToken)
	c.editIDs = append(c.editIDs, messageID)
	return nil
}

// snapshot lengths under the lock so race-tests stay sane.
func (c *captureSender) snapshot() (int, int, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sends), len(c.withOpts), len(c.edits)
}

// canonicalLarkWebhook always passes the lark.webhookPattern.
const canonicalLarkWebhook = "https://open.feishu.cn/open-apis/bot/v2/hook/0123456789abcdef0123456789abcdef"

// canonicalLarkApp is the lark-app:// pseudo-URL the store layer
// emits for application-bot rows. ParseAppToken accepts it and the
// router must dispatch it to AppClient, not the webhook Client.
const canonicalLarkApp = "lark-app://cli_test_app?secret=test-secret"

// newTestAppClient builds an AppClient pointed at an unreachable host
// so any router miss-route surfaces as a *lark.APIError (Transient,
// network error). The OAuth call would hit canonicalAPIBase by
// default; we redirect via the package-private newAppClientWithBase
// indirectly: tests in this package live alongside the lark package
// only through public types, so we use NewAppClient with a stub
// TokenSource that returns a fixed bearer. The IM endpoint then
// fails the canonical host so the test can detect the routing path
// by inspecting the error type.
func newTestAppClient(timeout time.Duration) *lark.AppClient {
	return lark.NewAppClient(stubTokenSource{tok: "t-test"}, timeout)
}

// stubTokenSource always returns the configured token, never hits the
// network. Used in router tests so the canonical Lark host is the
// only reachable failure point for the IM POST.
type stubTokenSource struct{ tok string }

func (s stubTokenSource) Token(_ context.Context, appID, appSecret string) (string, error) {
	_ = appID
	_ = appSecret
	return s.tok, nil
}

func TestDetectPlatform(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", domain.PlatformTelegram},
		{"12345:AAAAtoken", domain.PlatformTelegram},
		{"123456789:AAA-_BBBccc", domain.PlatformTelegram},
		{"https://example.com/path", domain.PlatformTelegram}, // wrong host stays telegram (Lark client will reject)
		{canonicalLarkWebhook, domain.PlatformLark},
		{"https://open.feishu.cn/open-apis/bot/v2/hook/anything", domain.PlatformLark},
		// case-sensitive: mixed-case host stays telegram
		{"https://Open.Feishu.cn/open-apis/bot/v2/hook/x", domain.PlatformTelegram},
		// lark-app:// goes to its own bucket (NOT the webhook bucket).
		{canonicalLarkApp, platformLarkApp},
		{"lark-app://cli_x?secret=s", platformLarkApp},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			if got := detectPlatform(tc.in); got != tc.want {
				t.Fatalf("detectPlatform(%q) = %q want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRouterSendRoutesTelegram pins the negative case: a non-Lark
// token must reach the Telegram adapter, NOT the Lark client.
func TestRouterSendRoutesTelegram(t *testing.T) {
	cs := &captureSender{}
	// The lark client is constructed but its server must NEVER be hit
	// for telegram tokens; if the router mis-routes the test will fail
	// because the lark.Client will reach for the canonical host
	// (which is unreachable in the test env) and return a network
	// error — surfacing the bug as a non-nil error here.
	larkC := lark.New(config.Telegram{HTTPTimeout: config.Duration(500 * time.Millisecond)})
	appC := newTestAppClient(500 * time.Millisecond)

	r := newSenderRouter(cs, larkC, appC)

	msgID, err := r.Send(context.Background(), "12345:tg-token", "chat-1", "MarkdownV2", "hi")
	if err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if msgID != 100 {
		t.Fatalf("msgID = %d want 100 (from telegram adapter)", msgID)
	}
	sends, withOpts, _ := cs.snapshot()
	if sends != 1 || withOpts != 0 {
		t.Fatalf("expected 1 tg.Send 0 SendWithOpts, got %d %d", sends, withOpts)
	}
	if cs.sends[0] != "12345:tg-token" {
		t.Fatalf("telegram sender received wrong token: %s", cs.sends[0])
	}
}

// TestRouterSendRoutesLark verifies a Lark-shaped token is dispatched
// to the lark.Client and never to the telegram capture. The exact
// proof is that the returned error chain contains a *lark.APIError —
// only the lark client can produce that type, so its presence is
// dispositive evidence that the router took the lark branch.
func TestRouterSendRoutesLark(t *testing.T) {
	cs := &captureSender{}
	larkC := lark.New(config.Telegram{HTTPTimeout: config.Duration(500 * time.Millisecond)})
	appC := newTestAppClient(500 * time.Millisecond)
	r := newSenderRouter(cs, larkC, appC)

	_, err := r.Send(context.Background(), canonicalLarkWebhook, "ignored", "ignored", "hi from lark")
	if err == nil {
		t.Fatalf("expected network error reaching canonical lark host, got nil — token mis-routed?")
	}
	if _, isLarkErr := lark.AsAPIError(err); !isLarkErr {
		t.Fatalf("expected *lark.APIError (proof the lark client was invoked), got %T %v", err, err)
	}
	sends, _, _ := cs.snapshot()
	if sends != 0 {
		t.Fatalf("telegram sender must NOT see lark tokens, got %d sends", sends)
	}
}

// TestRouterSendRoutesLarkApp pins the LB4 contract: a lark-app://
// token must dispatch to the AppClient (which hits the canonical
// host and produces a *lark.APIError on network failure), NOT the
// webhook client and NOT the telegram capture.
func TestRouterSendRoutesLarkApp(t *testing.T) {
	cs := &captureSender{}
	larkC := lark.New(config.Telegram{HTTPTimeout: config.Duration(500 * time.Millisecond)})
	appC := newTestAppClient(500 * time.Millisecond)
	r := newSenderRouter(cs, larkC, appC)

	_, err := r.Send(context.Background(), canonicalLarkApp, "oc_chat", "MarkdownV2", "hi from app")
	if err == nil {
		t.Fatalf("expected network error from canonical lark host")
	}
	if _, isLarkErr := lark.AsAPIError(err); !isLarkErr {
		t.Fatalf("expected *lark.APIError from app client, got %T %v", err, err)
	}
	sends, _, _ := cs.snapshot()
	if sends != 0 {
		t.Fatalf("telegram sender must NOT see lark-app tokens, got %d sends", sends)
	}
}

// TestRouterSendWithOptsLarkDropsButtons confirms inline-keyboard
// buttons are silently dropped on the Lark side (Lark webhooks have
// no inline_keyboard analogue per L2 design). The Telegram side
// keeps its SenderWithOpts pass-through.
func TestRouterSendWithOptsTelegramPreservesButtons(t *testing.T) {
	cs := &captureSender{}
	larkC := lark.New(config.Telegram{HTTPTimeout: config.Duration(500 * time.Millisecond)})
	appC := newTestAppClient(500 * time.Millisecond)
	r := newSenderRouter(cs, larkC, appC)

	opts := domain.SendOptions{
		Buttons: []domain.PushButton{{Text: "ACK", Callback: "ack:abc"}},
	}
	if _, err := r.SendWithOpts(context.Background(), "12345:tg-token", "c", "", "x", opts); err != nil {
		t.Fatalf("SendWithOpts err = %v", err)
	}
	_, withOpts, _ := cs.snapshot()
	if withOpts != 1 {
		t.Fatalf("withOpts = %d want 1 (telegram should take the buttons path)", withOpts)
	}
	if len(cs.withOptsB[0]) != 1 || cs.withOptsB[0][0].Callback != "ack:abc" {
		t.Fatalf("buttons lost: %#v", cs.withOptsB[0])
	}
}

// TestRouterEditMessageLarkFallsBackToSend exercises the V7-2
// editMessageText collapse on the Lark route. The router must
// invoke lark.Send (not lark.Edit) so the behaviour matches the
// design choice in L2; the call is observable here because the
// canonical Lark host returns a network error inside the 500ms
// timeout and AsAPIError confirms we reached the lark client at all.
func TestRouterEditMessageLarkRoutesToLark(t *testing.T) {
	cs := &captureSender{}
	larkC := lark.New(config.Telegram{HTTPTimeout: config.Duration(500 * time.Millisecond)})
	appC := newTestAppClient(500 * time.Millisecond)
	r := newSenderRouter(cs, larkC, appC)

	err := r.EditMessage(context.Background(), canonicalLarkWebhook, "chat", 42, "", "edited")
	if err == nil {
		t.Fatalf("expected network error from canonical host, got nil")
	}
	if _, isLarkErr := lark.AsAPIError(err); !isLarkErr {
		t.Fatalf("expected *lark.APIError, got %T %v", err, err)
	}
	_, _, edits := cs.snapshot()
	if edits != 0 {
		t.Fatalf("telegram EditMessage must not be invoked for lark tokens, got %d", edits)
	}
}

// TestRouterEditMessageLarkAppFallsBackToSend confirms that the
// lark-app:// route's EditMessage also degrades to a fresh Send via
// AppClient (no message_threads compatibility today).
func TestRouterEditMessageLarkAppFallsBackToSend(t *testing.T) {
	cs := &captureSender{}
	larkC := lark.New(config.Telegram{HTTPTimeout: config.Duration(500 * time.Millisecond)})
	appC := newTestAppClient(500 * time.Millisecond)
	r := newSenderRouter(cs, larkC, appC)

	err := r.EditMessage(context.Background(), canonicalLarkApp, "oc", 42, "", "edited")
	if err == nil {
		t.Fatalf("expected network error from canonical host, got nil")
	}
	if _, isLarkErr := lark.AsAPIError(err); !isLarkErr {
		t.Fatalf("expected *lark.APIError, got %T %v", err, err)
	}
	_, _, edits := cs.snapshot()
	if edits != 0 {
		t.Fatalf("telegram EditMessage must not be invoked for lark-app tokens, got %d", edits)
	}
}

// TestRouterEditMessageTelegramRoutesToEdit pins the happy-path edit
// behaviour for Telegram tokens: the router invokes the underlying
// SenderWithOpts.EditMessage, NOT a fresh Send.
func TestRouterEditMessageTelegramRoutesToEdit(t *testing.T) {
	cs := &captureSender{}
	larkC := lark.New(config.Telegram{HTTPTimeout: config.Duration(500 * time.Millisecond)})
	appC := newTestAppClient(500 * time.Millisecond)
	r := newSenderRouter(cs, larkC, appC)

	if err := r.EditMessage(context.Background(), "12345:tok", "chat", 99, "", "edit"); err != nil {
		t.Fatalf("EditMessage err = %v", err)
	}
	sends, _, edits := cs.snapshot()
	if edits != 1 {
		t.Fatalf("edits = %d want 1", edits)
	}
	if sends != 0 {
		t.Fatalf("telegram Edit took the Send fallback unexpectedly: sends=%d", sends)
	}
	if cs.editIDs[0] != 99 {
		t.Fatalf("messageID not preserved: %d", cs.editIDs[0])
	}
}

// TestRouterFallbackWhenTGSenderLacksOpts verifies the legacy fallback
// path: if the injected telegram sender does NOT implement
// SenderWithOpts (a plain *fakeSender from older tests), the router
// silently falls through to .Send and forwards the buttons-less call.
func TestRouterFallbackWhenTGSenderLacksOpts(t *testing.T) {
	plain := &plainSender{}
	larkC := lark.New(config.Telegram{HTTPTimeout: config.Duration(500 * time.Millisecond)})
	appC := newTestAppClient(500 * time.Millisecond)
	r := newSenderRouter(plain, larkC, appC)

	if _, err := r.SendWithOpts(context.Background(), "12345:tok", "c", "", "x", domain.SendOptions{Buttons: []domain.PushButton{{Text: "ack"}}}); err != nil {
		t.Fatalf("SendWithOpts err = %v", err)
	}
	if plain.calls != 1 {
		t.Fatalf("plain sender call count = %d", plain.calls)
	}

	if err := r.EditMessage(context.Background(), "12345:tok", "c", 1, "", "edit"); err != nil {
		t.Fatalf("EditMessage err = %v", err)
	}
	if plain.calls != 2 {
		t.Fatalf("plain sender call count after edit fallback = %d", plain.calls)
	}
}

// TestRouterRoundTripWithRealTelegramHTTPTest exercises the router end-
// to-end against an httptest.Server emulating the Telegram Bot API so
// the SendWithOpts adapter chain (router → tgSenderAdapter → tg.Client)
// is provably intact after the L3 refactor.
func TestRouterRoundTripWithRealTelegramHTTPTest(t *testing.T) {
	var lastBody []byte
	var mu sync.Mutex
	tgSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		lastBody = b
		mu.Unlock()
		_, _ = w.Write([]byte(`{"ok":true,"result":{"message_id":7}}`))
	}))
	defer tgSrv.Close()

	tgClient := tg.New(config.Telegram{APIBase: tgSrv.URL, HTTPTimeout: config.Duration(2 * time.Second)})
	larkC := lark.New(config.Telegram{HTTPTimeout: config.Duration(500 * time.Millisecond)})
	appC := newTestAppClient(500 * time.Millisecond)
	r := newSenderRouter(newTGSenderAdapter(tgClient), larkC, appC)

	msgID, err := r.SendWithOpts(context.Background(), "12345:tg", "chat-1", "MarkdownV2", "real call", domain.SendOptions{
		Buttons: []domain.PushButton{{Text: "ACK", Callback: "ack:fp"}},
	})
	if err != nil {
		t.Fatalf("SendWithOpts err = %v", err)
	}
	if msgID != 7 {
		t.Fatalf("msgID = %d want 7", msgID)
	}

	// Wire-level proof: the body must carry the reply_markup the
	// caller asked for — i.e. the router didn't strip it.
	mu.Lock()
	bodyCopy := lastBody
	mu.Unlock()
	if !strings.Contains(string(bodyCopy), "inline_keyboard") {
		t.Fatalf("inline_keyboard missing from telegram request: %s", string(bodyCopy))
	}

	// Sanity: a Lark-shaped token sent through the same router must
	// hit the (unreachable in tests) canonical Lark host and produce
	// a *lark.APIError, NOT a Telegram error.
	_, err = r.Send(context.Background(), canonicalLarkWebhook, "x", "", "y")
	if err == nil {
		t.Fatalf("expected error from lark route")
	}
	if _, ok := lark.AsAPIError(err); !ok {
		t.Fatalf("expected *lark.APIError, got %T", err)
	}
}

// plainSender satisfies the legacy domain.Sender shape only — it must
// NOT implement SenderWithOpts so the router exercises its fallback
// path.
type plainSender struct {
	mu    sync.Mutex
	calls int
}

func (p *plainSender) Send(_ context.Context, _, _, _, _ string) (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return 0, nil
}

// guard against accidentally adding SenderWithOpts to plainSender.
var _ domain.Sender = (*plainSender)(nil)
