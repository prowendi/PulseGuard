package tg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendi/pulseguard/internal/config"
)

// fakeServer wraps httptest.Server with a customisable response so tests
// can simulate every Telegram failure mode without touching the network.
type fakeServer struct {
	*httptest.Server
	lastBody   []byte
	lastPath   string
	lastMethod string
	handler    func(w http.ResponseWriter, r *http.Request)
}

func newFakeServer(handler func(w http.ResponseWriter, r *http.Request)) *fakeServer {
	fs := &fakeServer{handler: handler}
	fs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fs.lastBody = body
		fs.lastPath = r.URL.Path
		fs.lastMethod = r.Method
		fs.handler(w, r)
	}))
	return fs
}

func newClient(server string) *Client {
	return New(config.Telegram{APIBase: server, HTTPTimeout: config.Duration(2 * time.Second)})
}

func TestClassifySuccess(t *testing.T) {
	if err := Classify(200, []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestClassify429(t *testing.T) {
	body := `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":7}}`
	err := Classify(429, []byte(body))
	ae, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if ae.Class != Transient {
		t.Fatalf("class = %v", ae.Class)
	}
	if ae.RetryAfter != 7*time.Second {
		t.Fatalf("retry_after = %v", ae.RetryAfter)
	}
}

func TestClassify5xx(t *testing.T) {
	body := `{"ok":false,"error_code":502,"description":"Bad Gateway"}`
	err := Classify(502, []byte(body))
	ae, _ := err.(*APIError)
	if ae == nil || ae.Class != Transient {
		t.Fatalf("got %v", err)
	}
}

func TestClassify400ChatNotFound(t *testing.T) {
	body := `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`
	err := Classify(400, []byte(body))
	ae, _ := err.(*APIError)
	if ae == nil || ae.Class != PermanentClient {
		t.Fatalf("got %v", err)
	}
}

func TestClassify401(t *testing.T) {
	body := `{"ok":false,"error_code":401,"description":"Unauthorized"}`
	err := Classify(401, []byte(body))
	ae, _ := err.(*APIError)
	if ae == nil || ae.Class != PermanentServer {
		t.Fatalf("got %v", err)
	}
}

func TestClassify403Blocked(t *testing.T) {
	body := `{"ok":false,"error_code":403,"description":"Forbidden: bot was blocked by the user"}`
	err := Classify(403, []byte(body))
	ae, _ := err.(*APIError)
	if ae == nil || ae.Class != PermanentClient {
		t.Fatalf("got %v", err)
	}
}

func TestClassify403Other(t *testing.T) {
	body := `{"ok":false,"error_code":403,"description":"Forbidden: something else"}`
	err := Classify(403, []byte(body))
	ae, _ := err.(*APIError)
	if ae == nil || ae.Class != PermanentServer {
		t.Fatalf("got %v", err)
	}
}

func TestClassifyUnparseable(t *testing.T) {
	err := Classify(500, []byte("garbage not json"))
	ae, _ := err.(*APIError)
	if ae == nil || ae.Class != Transient {
		t.Fatalf("got %v", err)
	}
}

func TestSendSuccess(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	id, err := c.Send(context.Background(), "BOT:TOKEN", "12345", "MarkdownV2", "hello")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if id != 42 {
		t.Fatalf("msgID = %d", id)
	}
	if !strings.HasPrefix(srv.lastPath, "/botBOT:TOKEN/sendMessage") {
		t.Fatalf("path = %q", srv.lastPath)
	}
	if srv.lastMethod != http.MethodPost {
		t.Fatalf("method = %q", srv.lastMethod)
	}
	// Verify request body
	var req map[string]any
	if err := json.Unmarshal(srv.lastBody, &req); err != nil {
		t.Fatalf("body: %v", err)
	}
	if req["chat_id"] != "12345" || req["text"] != "hello" || req["parse_mode"] != "MarkdownV2" {
		t.Fatalf("body = %v", req)
	}
}

func TestSendOmitsNoneParseMode(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	if _, err := c.Send(context.Background(), "T", "C", "None", "x"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(srv.lastBody, &req); err != nil {
		t.Fatalf("body: %v", err)
	}
	if _, present := req["parse_mode"]; present {
		t.Fatalf("parse_mode should be omitted, got %v", req)
	}
}

func TestSend429RetryAfter(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":3}}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	_, err := c.Send(context.Background(), "T", "C", "", "x")
	ae, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("expected APIError, got %T %v", err, err)
	}
	if ae.Class != Transient {
		t.Fatalf("class = %v", ae.Class)
	}
	if ae.RetryAfter != 3*time.Second {
		t.Fatalf("retry_after = %v", ae.RetryAfter)
	}
}

func TestSend400ChatNotFound(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	_, err := c.Send(context.Background(), "T", "C", "", "x")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != PermanentClient {
		t.Fatalf("got %v", err)
	}
}

func TestSend401(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	_, err := c.Send(context.Background(), "T", "C", "", "x")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != PermanentServer {
		t.Fatalf("got %v", err)
	}
}

func TestSend500Transient(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":503,"description":"Service Unavailable"}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	_, err := c.Send(context.Background(), "T", "C", "", "x")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != Transient {
		t.Fatalf("got %v", err)
	}
}

func TestSendUnparseableBody(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		_, _ = io.WriteString(w, "not json")
	})
	defer srv.Close()
	c := newClient(srv.URL)
	_, err := c.Send(context.Background(), "T", "C", "", "x")
	ae, ok := AsAPIError(err)
	if !ok || ae.Class != Transient {
		t.Fatalf("got %v", err)
	}
}

func TestSendNetworkErrorTransient(t *testing.T) {
	// Closed server simulates network failure.
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {})
	srv.Close()
	c := newClient(srv.URL)
	_, err := c.Send(context.Background(), "T", "C", "", "x")
	ae, ok := AsAPIError(err)
	if !ok {
		t.Fatalf("expected APIError, got %T %v", err, err)
	}
	if ae.Class != Transient {
		t.Fatalf("class = %v desc=%v", ae.Class, ae.Description)
	}
}

func TestSendValidates(t *testing.T) {
	c := newClient("http://example.invalid")
	if _, err := c.Send(context.Background(), "", "C", "", "x"); err == nil {
		t.Fatalf("expected empty token error")
	}
	if _, err := c.Send(context.Background(), "T", "", "", "x"); err == nil {
		t.Fatalf("expected empty chat error")
	}
}

func TestAPIErrorMessage(t *testing.T) {
	e := &APIError{Class: Transient, Code: 429, Description: "Too Many", RetryAfter: 5 * time.Second}
	msg := e.Error()
	if !strings.Contains(msg, "transient") || !strings.Contains(msg, "429") {
		t.Fatalf("msg = %q", msg)
	}
	if !errors.Is(error(e), e) {
		t.Fatalf("identity")
	}
}

// Ensure the APIError type can round-trip through fmt for slog use.
func TestAPIErrorFormatV(t *testing.T) {
	e := &APIError{Class: PermanentClient, Code: 400, Description: "bad"}
	if got := fmt.Sprintf("%s", e); !strings.Contains(got, "bad") {
		t.Fatalf("got %q", got)
	}
}
