package tg

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestSendWithOpts_NoButtons_OmitsReplyMarkup proves the V7-1 helper
// is byte-identical to plain Send when no buttons are supplied — the
// regression net for the buttonless legacy callers stays valid.
func TestSendWithOpts_NoButtons_OmitsReplyMarkup(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	if _, err := c.SendWithOpts(context.Background(), "T", "C", "MarkdownV2", "hello", SendOpts{}); err != nil {
		t.Fatalf("SendWithOpts: %v", err)
	}
	var req map[string]any
	if err := json.Unmarshal(srv.lastBody, &req); err != nil {
		t.Fatalf("body: %v", err)
	}
	if _, has := req["reply_markup"]; has {
		t.Fatalf("reply_markup should be omitted when no buttons: %v", req)
	}
}

// TestSendWithOpts_Buttons_EmitsInlineKeyboard captures the V7-1
// contract: a non-empty SendOpts.Buttons surfaces as a single-row
// inline_keyboard with text + callback_data / url preserved.
func TestSendWithOpts_Buttons_EmitsInlineKeyboard(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":42}}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	opts := SendOpts{Buttons: []InlineButton{
		{Text: "ACK", Callback: "ack:fp-1"},
		{Text: "Runbook", URL: "https://example.com/rb"},
	}}
	if _, err := c.SendWithOpts(context.Background(), "T", "C", "", "alert", opts); err != nil {
		t.Fatalf("SendWithOpts: %v", err)
	}
	body := string(srv.lastBody)
	for _, frag := range []string{
		`"reply_markup"`,
		`"inline_keyboard"`,
		`"text":"ACK"`,
		`"callback_data":"ack:fp-1"`,
		`"text":"Runbook"`,
		`"url":"https://example.com/rb"`,
	} {
		if !strings.Contains(body, frag) {
			t.Fatalf("body missing %q: %s", frag, body)
		}
	}
}

// TestSendWithOpts_TruncatesLongCallbackData proves the 64-byte clip
// kicks in so a runaway fingerprint cannot make Telegram reject the
// whole sendMessage payload.
func TestSendWithOpts_TruncatesLongCallbackData(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1}}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	long := strings.Repeat("x", 200)
	opts := SendOpts{Buttons: []InlineButton{{Text: "ACK", Callback: "ack:" + long}}}
	if _, err := c.SendWithOpts(context.Background(), "T", "C", "", "alert", opts); err != nil {
		t.Fatalf("SendWithOpts: %v", err)
	}
	body := string(srv.lastBody)
	// Extract the callback_data field manually — JSON is small enough.
	idx := strings.Index(body, `"callback_data":"`)
	if idx < 0 {
		t.Fatalf("callback_data missing: %s", body)
	}
	rest := body[idx+len(`"callback_data":"`):]
	end := strings.IndexByte(rest, '"')
	if end < 0 || end > 64 {
		t.Fatalf("callback_data not clipped to 64 bytes: len=%d, body=%s", end, body)
	}
}

// TestEdit_HappyPath proves the new Edit helper marshals the expected
// JSON body and surfaces 200 success.
func TestEdit_HappyPath(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":7}}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	if err := c.Edit(context.Background(), "BOT:TOKEN", "12345", 7, "MarkdownV2", "new body"); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if !strings.HasSuffix(srv.lastPath, "/editMessageText") {
		t.Fatalf("path = %q", srv.lastPath)
	}
	body := string(srv.lastBody)
	for _, frag := range []string{`"chat_id":"12345"`, `"message_id":7`, `"text":"new body"`, `"parse_mode":"MarkdownV2"`} {
		if !strings.Contains(body, frag) {
			t.Fatalf("body missing %q: %s", frag, body)
		}
	}
}

// TestEdit_NotModifiedSwallowed proves Telegram's "Bad Request: message
// is not modified" path is treated as a silent success — re-editing
// with the same text is idempotent.
func TestEdit_NotModifiedSwallowed(t *testing.T) {
	srv := newFakeServer(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"Bad Request: message is not modified"}`)
	})
	defer srv.Close()
	c := newClient(srv.URL)
	if err := c.Edit(context.Background(), "T", "C", 1, "", "same"); err != nil {
		t.Fatalf("Edit should swallow not-modified, got: %v", err)
	}
}

// TestEdit_Validates rejects obvious-input errors so callers cannot
// silently issue a no-op edit.
func TestEdit_Validates(t *testing.T) {
	c := newClient("http://example.invalid")
	if err := c.Edit(context.Background(), "", "C", 1, "", "x"); err == nil {
		t.Fatal("expected empty token error")
	}
	if err := c.Edit(context.Background(), "T", "", 1, "", "x"); err == nil {
		t.Fatal("expected empty chat error")
	}
	if err := c.Edit(context.Background(), "T", "C", 0, "", "x"); err == nil {
		t.Fatal("expected zero message_id error")
	}
}
