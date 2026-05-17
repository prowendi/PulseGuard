package telegram

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// editSnapshot returns a copy of every editMessageText body the fake
// Telegram server has received so callback_query tests can assert the
// "@user 已 ACK" echo was emitted.
func (f *fakeTG) editSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.editCalls))
	copy(out, f.editCalls)
	return out
}

// TestListener_CallbackQueryAckHappyPath wires a callback_query update
// carrying "ack:fp-cb" and asserts:
//   1. AlertAcker.Insert was called with the expected fingerprint /
//      acked_by label (the V7-1 spec test).
//   2. editMessageText was issued, appending the "已 ACK" suffix to the
//      original alert body so every chat participant sees the claim.
//   3. answerCallbackQuery was called to clear the loading spinner.
func TestListener_CallbackQueryAckHappyPath(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	// callback_query with the V7-1 "ack:<fp>" data convention plus an
	// embedded message so the listener can target the editMessageText
	// at the original alert.
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 11,
		"callback_query": {
			"id": "cb-1",
			"from": {"id": 555, "is_bot": false, "username": "alice"},
			"message": {
				"chat": {"id": 950},
				"message_id": 7700,
				"text": "ALERT: db01 down"
			},
			"data": "ack:fp-cb"
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
	if call.Fingerprint != "fp-cb" {
		t.Fatalf("fingerprint = %q want fp-cb", call.Fingerprint)
	}
	if call.AckedBy != "@alice" {
		t.Fatalf("acked_by = %q want @alice", call.AckedBy)
	}
	if call.BotID != 42 {
		t.Fatalf("bot_id = %d want 42 (DB bot.ID)", call.BotID)
	}
	if call.ChatID != "950" {
		t.Fatalf("chat_id = %q want \"950\"", call.ChatID)
	}

	// editMessageText must have echoed the ACK banner into the
	// original message body.
	eventually(t, 3*time.Second, func() bool { return len(srv.editSnapshot()) >= 1 })
	edits := srv.editSnapshot()
	if !strings.Contains(edits[0], "ALERT: db01 down") {
		t.Fatalf("edit body missing original alert: %s", edits[0])
	}
	if !strings.Contains(edits[0], "@alice") || !strings.Contains(edits[0], "ACK") {
		t.Fatalf("edit body missing ACK banner: %s", edits[0])
	}
	if !strings.Contains(edits[0], `"message_id":7700`) {
		t.Fatalf("edit body missing message_id=7700: %s", edits[0])
	}

	// answerCallbackQuery must have closed the spinner.
	eventually(t, 3*time.Second, func() bool { return atomic.LoadInt32(&srv.answerCalls) >= 1 })
}

// TestListener_CallbackQueryUnknownPrefixIgnored proves a callback
// whose data does NOT start with "ack:" is a silent no-op (the
// acker is not called) yet answerCallbackQuery still fires so the
// user's client clears the loading spinner.
func TestListener_CallbackQueryUnknownPrefixIgnored(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 12,
		"callback_query": {
			"id": "cb-2",
			"message": {"chat": {"id": 951}, "message_id": 1, "text": "x"},
			"data": "menu:open"
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

	eventually(t, 3*time.Second, func() bool { return atomic.LoadInt32(&srv.answerCalls) >= 1 })
	if got := len(ack.Calls()); got != 0 {
		t.Fatalf("acker fired %d times for unknown prefix, want 0", got)
	}
	if got := len(srv.editSnapshot()); got != 0 {
		t.Fatalf("editMessageText fired %d times for unknown prefix, want 0", got)
	}
}

// TestListener_CallbackQueryChatFallback exercises the AckedBy
// fallback path: callback_query.from is nil so the audit row carries
// "chat:<chat_id>" rather than an empty string.
func TestListener_CallbackQueryChatFallback(t *testing.T) {
	srv := newFakeTG()
	defer srv.Close()
	srv.queueUpdates(`{"ok":true,"result":[{
		"update_id": 13,
		"callback_query": {
			"id": "cb-3",
			"message": {"chat": {"id": 952}, "message_id": 12, "text": "alert"},
			"data": "ack:fp-chatfallback"
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
	if got := ack.Calls()[0].AckedBy; got != "chat:952" {
		t.Fatalf("AckedBy = %q want chat:952", got)
	}
}
