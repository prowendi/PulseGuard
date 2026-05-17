package lark

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeServer wraps httptest.Server with the customisable handler each
// test case needs. The lastBody / lastPath / lastHeaders fields are
// inspected after the call to assert the wire shape.
type fakeServer struct {
	*httptest.Server
	lastBody    []byte
	lastPath    string
	lastMethod  string
	lastCType   string
	handler     func(w http.ResponseWriter, r *http.Request)
	callCount   int
}

func newFakeServer(handler func(w http.ResponseWriter, r *http.Request)) *fakeServer {
	fs := &fakeServer{handler: handler}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fs.lastBody = body
		fs.lastPath = r.URL.Path
		fs.lastMethod = r.Method
		fs.lastCType = r.Header.Get("Content-Type")
		fs.callCount++
		fs.handler(w, r)
	}))
	return fs
}

// canonical webhook used across tests. The 32-char key shape matches
// the real Lark output but the value itself is a placeholder so the
// fixture cannot be confused with a leaked credential.
const testWebhook = "https://open.feishu.cn/open-apis/bot/v2/hook/0123456789abcdef0123456789abcdef"

func newTestClient(server *fakeServer) *Client {
	return newWithBase(server.URL, 2*time.Second)
}

func TestSendSuccess(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"success","data":{}}`))
	})
	defer srv.Close()

	c := newTestClient(srv)
	msgID, err := c.Send(context.Background(), testWebhook, "ignored-chat", "ignored-mode", "hello lark")
	if err != nil {
		t.Fatalf("Send err = %v", err)
	}
	if msgID != 0 {
		t.Fatalf("msgID = %d, want 0 (lark webhooks return no id)", msgID)
	}

	// Wire-shape assertions: path, method, content-type, body.
	if srv.lastMethod != http.MethodPost {
		t.Fatalf("method = %s", srv.lastMethod)
	}
	wantPath := "/open-apis/bot/v2/hook/0123456789abcdef0123456789abcdef"
	if srv.lastPath != wantPath {
		t.Fatalf("path = %s, want %s", srv.lastPath, wantPath)
	}
	if srv.lastCType != "application/json" {
		t.Fatalf("content-type = %s", srv.lastCType)
	}

	var sent sendReq
	if err := json.Unmarshal(srv.lastBody, &sent); err != nil {
		t.Fatalf("unmarshal body: %v body=%s", err, string(srv.lastBody))
	}
	if sent.MsgType != "text" {
		t.Fatalf("msg_type = %s", sent.MsgType)
	}
	if sent.Content.Text != "hello lark" {
		t.Fatalf("content.text = %s", sent.Content.Text)
	}
}

// TestSendSuccessLegacyEnvelope covers the older webhook response
// shape ({"StatusCode":0,"StatusMessage":"success"}) that some Lark
// tenants still receive.
func TestSendSuccessLegacyEnvelope(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"StatusCode":0,"StatusMessage":"success"}`))
	})
	defer srv.Close()

	c := newTestClient(srv)
	if _, err := c.Send(context.Background(), testWebhook, "", "", "ok"); err != nil {
		t.Fatalf("Send err = %v", err)
	}
}

func TestSendNonZeroCode(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		// 9499 = invalid webhook key (Lark documented code)
		_, _ = w.Write([]byte(`{"code":9499,"msg":"invalid webhook"}`))
	})
	defer srv.Close()

	c := newTestClient(srv)
	_, err := c.Send(context.Background(), testWebhook, "", "", "boom")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	ae, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("expected *APIError, got %T %v", err, err)
	}
	if ae.Class != PermanentClient {
		t.Fatalf("class = %v want PermanentClient", ae.Class)
	}
	if ae.Code != 9499 {
		t.Fatalf("code = %d", ae.Code)
	}
	if !strings.Contains(ae.Description, "9499") {
		t.Fatalf("description missing code: %q", ae.Description)
	}
}

func TestSendHTTP5xx(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`upstream down`))
	})
	defer srv.Close()
	c := newTestClient(srv)
	_, err := c.Send(context.Background(), testWebhook, "", "", "x")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != Transient || ae.Code != 503 {
		t.Fatalf("got %v", err)
	}
}

func TestSendHTTP429(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"code":99991,"msg":"rate limit"}`))
	})
	defer srv.Close()
	c := newTestClient(srv)
	_, err := c.Send(context.Background(), testWebhook, "", "", "x")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != Transient {
		t.Fatalf("got %v", err)
	}
	if !ae.IsTransient() {
		t.Fatalf("not transient: %v", err)
	}
}

func TestSendHTTP400(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"code":9499,"msg":"invalid token"}`))
	})
	defer srv.Close()
	c := newTestClient(srv)
	_, err := c.Send(context.Background(), testWebhook, "", "", "x")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != PermanentClient {
		t.Fatalf("got %v", err)
	}
}

func TestSendBadWebhook(t *testing.T) {
	// No httptest server needed — the regex check runs before any
	// network call. A telegram-shaped token must fail loudly.
	c := newTestClient(newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("network call must not happen for malformed webhook")
	}))
	defer c.httpC.CloseIdleConnections()

	cases := []string{
		"",
		"12345:AAAAtelegramToken",                                      // telegram shape
		"http://open.feishu.cn/open-apis/bot/v2/hook/key",              // http, not https
		"https://example.com/open-apis/bot/v2/hook/key",                // wrong host
		"https://open.feishu.cn/open-apis/bot/v1/hook/key",             // wrong path version
		"https://open.feishu.cn/open-apis/bot/v2/hook/",                // empty key
		"https://open.feishu.cn/open-apis/bot/v2/hook/key?extra=param", // query string
	}
	for _, tok := range cases {
		tok := tok
		t.Run(tok, func(t *testing.T) {
			_, err := c.Send(context.Background(), tok, "", "", "x")
			if err == nil {
				t.Fatalf("expected ErrBadWebhook for %q, got nil", tok)
			}
			if err != ErrBadWebhook {
				// AsAPIError must NOT match — ErrBadWebhook is a
				// sentinel, not an *APIError.
				if _, ok := AsAPIError(err); ok {
					t.Fatalf("got *APIError, want ErrBadWebhook sentinel")
				}
				t.Fatalf("got %v, want ErrBadWebhook", err)
			}
		})
	}
}

// TestWebhookPatternAcceptsTrailingSlash documents that a copy-paste
// from the Lark UI with a trailing slash is still considered valid —
// the regex tolerates an optional final "/" so operators don't have
// to remember to strip it.
func TestWebhookPatternAcceptsTrailingSlash(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	})
	defer srv.Close()
	c := newTestClient(srv)
	if _, err := c.Send(context.Background(), testWebhook+"/", "", "", "x"); err != nil {
		t.Fatalf("trailing slash should be accepted: %v", err)
	}
}

// TestEditFallsBackToSend asserts the SenderWithOpts compatibility
// shim issues a fresh Send rather than failing — this is the design
// choice in L2 so editMessageText state-machine collapse degrades to
// "new message" instead of throwing in Lark groups.
func TestEditFallsBackToSend(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
	})
	defer srv.Close()

	c := newTestClient(srv)
	if err := c.Edit(context.Background(), testWebhook, "ignored", 42, "MarkdownV2", "edited body"); err != nil {
		t.Fatalf("Edit err = %v", err)
	}
	if srv.callCount != 1 {
		t.Fatalf("callCount = %d, want 1 (Edit -> exactly one Send)", srv.callCount)
	}
	var sent sendReq
	if err := json.Unmarshal(srv.lastBody, &sent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if sent.Content.Text != "edited body" {
		t.Fatalf("text = %s", sent.Content.Text)
	}
}

// TestEditPropagatesBadWebhook ensures the fallback still surfaces
// validation failures cleanly so the worker DLQs them rather than
// silently swallowing a misconfigured token on the edit path.
func TestEditPropagatesBadWebhook(t *testing.T) {
	c := newTestClient(newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("must not call server with malformed webhook")
	}))
	err := c.Edit(context.Background(), "12345:bogus", "x", 1, "", "y")
	if err != ErrBadWebhook {
		t.Fatalf("got %v want ErrBadWebhook", err)
	}
}

func TestClassString(t *testing.T) {
	if Transient.String() != "transient" {
		t.Fatalf("Transient string = %s", Transient.String())
	}
	if PermanentClient.String() != "permanent_client" {
		t.Fatalf("PermanentClient string = %s", PermanentClient.String())
	}
	if Class(99).String() != "unknown" {
		t.Fatalf("unknown bucket missing")
	}
}

func TestAPIErrorNil(t *testing.T) {
	var e *APIError
	if got := e.Error(); got != "<nil lark.APIError>" {
		t.Fatalf("nil APIError = %s", got)
	}
}

func TestIsLarkEnvelope(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"plain text", "hello world", false},
		{"empty", "", false},
		{"whitespace", "   \n\t  ", false},
		{"json without msg_type", `{"text":"hi"}`, false},
		{"json msg_type empty", `{"msg_type":""}`, false},
		{"text envelope", `{"msg_type":"text","content":{"text":"hi"}}`, true},
		{"post envelope", `{"msg_type":"post","content":{"post":{}}}`, true},
		{"interactive envelope", `{"msg_type":"interactive","card":{}}`, true},
		{"leading whitespace", "  \n  {\"msg_type\":\"text\"}", true},
		{"broken json", `{"msg_type":`, false},
		{"non-object", `["msg_type"]`, false},
	}
	for _, c := range cases {
		if got := isLarkEnvelope(c.in); got != c.want {
			t.Errorf("%s: isLarkEnvelope(%q) = %v want %v", c.name, c.in, got, c.want)
		}
	}
}

func TestBuildLarkBody_TextWrapsPlainString(t *testing.T) {
	body, err := buildLarkBody("hello")
	if err != nil {
		t.Fatalf("buildLarkBody: %v", err)
	}
	var got sendReq
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.MsgType != "text" {
		t.Errorf("MsgType = %q want text", got.MsgType)
	}
	if got.Content.Text != "hello" {
		t.Errorf("Content.Text = %q want hello", got.Content.Text)
	}
}

func TestBuildLarkBody_EnvelopePassThrough(t *testing.T) {
	envelope := `{"msg_type":"post","content":{"post":{"zh_cn":{"title":"t","content":[]}}}}`
	body, err := buildLarkBody(envelope)
	if err != nil {
		t.Fatalf("buildLarkBody: %v", err)
	}
	if string(body) != envelope {
		t.Fatalf("envelope mutated:\n got:  %s\n want: %s", body, envelope)
	}
}

func TestSendInteractiveCardPassesThrough(t *testing.T) {
	// Verify the body the server receives is the exact envelope the
	// template emitted — no double-wrapping into a "text" message.
	want := `{"msg_type":"interactive","card":{"elements":[{"tag":"div","text":{"tag":"plain_text","content":"ok"}}]}}`
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"msg":"success"}`))
	})
	defer srv.Close()
	c := newTestClient(srv)
	if _, err := c.Send(context.Background(), testWebhook, "", "", want); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := string(srv.lastBody); got != want {
		t.Fatalf("body mismatch:\n got:  %s\n want: %s", got, want)
	}
}
