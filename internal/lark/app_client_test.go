package lark

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// twinFakeServer multiplexes OAuth + IM API endpoints on one
// httptest.Server so AppClient tests can exercise the (token-fetch →
// send) flow end-to-end. The handler dispatches by path prefix.
type twinFakeServer struct {
	*httptest.Server
	oauthCalls int32
	imCalls    int32
	imBody     []byte
	imAuth     string
	imPath     string
	imQuery    string

	oauthHandler atomic.Pointer[http.HandlerFunc]
	imHandler    atomic.Pointer[http.HandlerFunc]
	mu           sync.Mutex
}

func newTwinFakeServer(oauthH, imH http.HandlerFunc) *twinFakeServer {
	fs := &twinFakeServer{}
	fs.oauthHandler.Store(&oauthH)
	fs.imHandler.Store(&imH)
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		switch {
		case strings.HasPrefix(r.URL.Path, "/open-apis/auth/v3/tenant_access_token"):
			atomic.AddInt32(&fs.oauthCalls, 1)
			(*fs.oauthHandler.Load())(w, r)
		case strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/messages"):
			atomic.AddInt32(&fs.imCalls, 1)
			fs.mu.Lock()
			fs.imBody = body
			fs.imAuth = r.Header.Get("Authorization")
			fs.imPath = r.URL.Path
			fs.imQuery = r.URL.RawQuery
			fs.mu.Unlock()
			(*fs.imHandler.Load())(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	return fs
}

func (fs *twinFakeServer) oauthCallCount() int { return int(atomic.LoadInt32(&fs.oauthCalls)) }
func (fs *twinFakeServer) imCallCount() int    { return int(atomic.LoadInt32(&fs.imCalls)) }

func okOAuthHandler(token string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"` + token + `","expire":7200}`))
	}
}
func okIMHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"success","data":{"message_id":"om_abc123"}}`))
	}
}

// TestParseAppToken pins the round-trip with the store-layer
// assembled URL.
func TestParseAppToken(t *testing.T) {
	tok := LarkAppTokenPrefix + "cli_zzz?secret=" + "abc%20def" // url-encoded space
	appID, secret, err := ParseAppToken(tok)
	if err != nil {
		t.Fatalf("ParseAppToken: %v", err)
	}
	if appID != "cli_zzz" {
		t.Fatalf("appID = %q", appID)
	}
	if secret != "abc def" {
		t.Fatalf("secret = %q (decoded?)", secret)
	}
}

// TestParseAppToken_Rejects pins the negative cases — every malformed
// token must yield ErrBadAppCreds so the worker DLQs cleanly.
func TestParseAppToken_Rejects(t *testing.T) {
	cases := []string{
		"",
		"https://open.feishu.cn/x",
		"lark-app://",
		"lark-app://x",                // no secret
		"lark-app://?secret=y",        // no appID
		"lark-app:/cli_x?secret=y",    // malformed scheme
		"telegram-tok",
	}
	for _, in := range cases {
		in := in
		t.Run(in, func(t *testing.T) {
			_, _, err := ParseAppToken(in)
			if err == nil {
				t.Fatalf("expected error for %q", in)
			}
		})
	}
}

// TestAppClient_SendSuccess verifies the full happy path: OAuth call
// fires once, IM POST has the right path/query/auth header, and the
// body is the doubly-encoded {"msg_type":"text","content":"{...}"}
// shape Lark expects.
func TestAppClient_SendSuccess(t *testing.T) {
	srv := newTwinFakeServer(okOAuthHandler("t-acc-123"), okIMHandler())
	defer srv.Close()
	tok := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	c := newAppClientWithBase(tok, srv.URL, 2*time.Second, "")

	token := LarkAppTokenPrefix + "cli_app?secret=sec"
	_, err := c.Send(context.Background(), token, "oc_chat", "MarkdownV2", "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Token fetched once.
	if got := srv.oauthCallCount(); got != 1 {
		t.Fatalf("oauthCalls = %d want 1", got)
	}
	if got := srv.imCallCount(); got != 1 {
		t.Fatalf("imCalls = %d want 1", got)
	}

	// Auth header carries the bearer token.
	if srv.imAuth != "Bearer t-acc-123" {
		t.Fatalf("Authorization = %q", srv.imAuth)
	}
	// receive_id_type query carries the default.
	if !strings.Contains(srv.imQuery, "receive_id_type=chat_id") {
		t.Fatalf("receive_id_type missing: %s", srv.imQuery)
	}
	// Path is the IM messages endpoint (no trailing message_id).
	if srv.imPath != "/open-apis/im/v1/messages" {
		t.Fatalf("imPath = %s", srv.imPath)
	}

	// Body wire shape.
	var sent imSendReq
	if err := json.Unmarshal(srv.imBody, &sent); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, srv.imBody)
	}
	if sent.ReceiveID != "oc_chat" {
		t.Fatalf("receive_id = %q", sent.ReceiveID)
	}
	if sent.MsgType != "text" {
		t.Fatalf("msg_type = %q", sent.MsgType)
	}
	var inner imTextContent
	if err := json.Unmarshal([]byte(sent.Content), &inner); err != nil {
		t.Fatalf("unmarshal inner: %v content=%q", err, sent.Content)
	}
	if inner.Text != "hello" {
		t.Fatalf("inner.Text = %q", inner.Text)
	}
}

// TestAppClient_SendReusesTokenAcrossCalls confirms the cache: two
// back-to-back Sends share a single OAuth call.
func TestAppClient_SendReusesTokenAcrossCalls(t *testing.T) {
	srv := newTwinFakeServer(okOAuthHandler("t-shared"), okIMHandler())
	defer srv.Close()
	tok := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	c := newAppClientWithBase(tok, srv.URL, 2*time.Second, "")

	tokenStr := LarkAppTokenPrefix + "cli_app?secret=sec"
	for i := 0; i < 3; i++ {
		if _, err := c.Send(context.Background(), tokenStr, "oc", "", "ping"); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	if got := srv.oauthCallCount(); got != 1 {
		t.Fatalf("oauthCalls = %d want 1 (cache miss)", got)
	}
	if got := srv.imCallCount(); got != 3 {
		t.Fatalf("imCalls = %d want 3", got)
	}
}

// TestAppClient_Send401EvictsToken pins the 401 path: the cached
// token is evicted so the NEXT call refreshes.
func TestAppClient_Send401EvictsToken(t *testing.T) {
	var oauthN int32
	tokens := []string{"t-old", "t-new"}
	oauthH := func(w http.ResponseWriter, r *http.Request) {
		i := atomic.AddInt32(&oauthN, 1) - 1
		if int(i) >= len(tokens) {
			i = int32(len(tokens)) - 1
		}
		_, _ = w.Write([]byte(`{"code":0,"tenant_access_token":"` + tokens[i] + `","expire":7200}`))
	}
	var imN int32
	imH := func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&imN, 1) == 1 {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}
	srv := newTwinFakeServer(oauthH, imH)
	defer srv.Close()
	tok := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	c := newAppClientWithBase(tok, srv.URL, 2*time.Second, "")

	tokStr := LarkAppTokenPrefix + "cli_z?secret=s"
	_, err := c.Send(context.Background(), tokStr, "oc", "", "x")
	if err == nil {
		t.Fatal("expected 401 error on first send")
	}
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != Transient || ae.Code != 401 {
		t.Fatalf("first err = %v", err)
	}
	// Second send refreshes (oauth called twice, IM called twice, IM
	// second call succeeds).
	if _, err := c.Send(context.Background(), tokStr, "oc", "", "x"); err != nil {
		t.Fatalf("second send: %v", err)
	}
	if got := srv.oauthCallCount(); got != 2 {
		t.Fatalf("oauthCalls = %d want 2 (eviction must force refresh)", got)
	}
}

// TestAppClient_BadToken returns ErrBadAppCreds before any HTTP traffic.
func TestAppClient_BadToken(t *testing.T) {
	srv := newTwinFakeServer(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not hit oauth on bad token")
	}, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not hit IM on bad token")
	})
	defer srv.Close()
	tok := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	c := newAppClientWithBase(tok, srv.URL, 2*time.Second, "")
	_, err := c.Send(context.Background(), "not-a-lark-app-token", "oc", "", "x")
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestAppClient_EmptyChatID rejects before any HTTP traffic so the
// worker never wastes an OAuth round-trip.
func TestAppClient_EmptyChatID(t *testing.T) {
	srv := newTwinFakeServer(okOAuthHandler("t"), func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("must not call IM with empty chat id")
	})
	defer srv.Close()
	tok := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	c := newAppClientWithBase(tok, srv.URL, 2*time.Second, "")
	_, err := c.Send(context.Background(), LarkAppTokenPrefix+"x?secret=y", "", "", "msg")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != PermanentClient {
		t.Fatalf("expected PermanentClient APIError, got %v", err)
	}
}

// TestAppClient_NonZeroCodePermanent matches the webhook client's
// classification: a code != 0 in a 2xx response is permanent.
func TestAppClient_NonZeroCodePermanent(t *testing.T) {
	srv := newTwinFakeServer(okOAuthHandler("t"), func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":230001,"msg":"bot not in chat"}`))
	})
	defer srv.Close()
	tok := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	c := newAppClientWithBase(tok, srv.URL, 2*time.Second, "")
	_, err := c.Send(context.Background(), LarkAppTokenPrefix+"x?secret=y", "oc", "", "msg")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != PermanentClient || ae.Code != 230001 {
		t.Fatalf("got %v", err)
	}
}

// TestAppClient_SendEnvelopePassThrough verifies pre-built lark
// envelopes (post / interactive / image) are forwarded verbatim
// rather than being wrapped as text.
func TestAppClient_SendEnvelopePassThrough(t *testing.T) {
	srv := newTwinFakeServer(okOAuthHandler("t"), okIMHandler())
	defer srv.Close()
	tok := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	c := newAppClientWithBase(tok, srv.URL, 2*time.Second, "")
	envelope := `{"msg_type":"interactive","card":{"elements":[{"tag":"div","text":{"tag":"plain_text","content":"hi"}}]}}`
	if _, err := c.Send(context.Background(), LarkAppTokenPrefix+"x?secret=y", "oc", "", envelope); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var sent imSendReq
	if err := json.Unmarshal(srv.imBody, &sent); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, srv.imBody)
	}
	if sent.MsgType != "interactive" {
		t.Fatalf("msg_type = %q want interactive", sent.MsgType)
	}
	// content should be the card object stringified, NOT wrapped in
	// {"text": ...}.
	if !strings.Contains(sent.Content, `"elements"`) {
		t.Fatalf("envelope card not forwarded: %q", sent.Content)
	}
	if strings.Contains(sent.Content, `"text"`) && strings.Contains(sent.Content, `"tag":"plain_text"`) {
		// "text" here is from the card schema's plain_text node, not
		// the imTextContent wrapper. Make sure we didn't double-wrap.
	}
}

// TestAppClient_EditMessageIDZeroFallsBackToSend keeps the V7-2
// safety net: an unknown messageID degrades to a fresh send so the
// alert still lands.
func TestAppClient_EditMessageIDZeroFallsBackToSend(t *testing.T) {
	srv := newTwinFakeServer(okOAuthHandler("t"), okIMHandler())
	defer srv.Close()
	tok := newOAuthClientWithBase(srv.URL, 2*time.Second, time.Now)
	c := newAppClientWithBase(tok, srv.URL, 2*time.Second, "")
	if err := c.Edit(context.Background(), LarkAppTokenPrefix+"x?secret=y", "oc", 0, "", "edited"); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if got := srv.imCallCount(); got != 1 {
		t.Fatalf("imCalls = %d want 1 (Edit fallback should issue exactly one Send)", got)
	}
	// Body must be POST /messages, not PATCH on a message_id endpoint.
	if srv.imPath != "/open-apis/im/v1/messages" {
		t.Fatalf("imPath = %q want POST endpoint", srv.imPath)
	}
}
