// Package runtime — sender_adapter bridges *tg.Client (which lives in
// internal/tg and cannot import internal/domain without a cycle: domain
// is imported by tg's callers, not by tg itself) to the
// domain.SenderWithOpts interface the V7 worker consumes for inline
// keyboards and editMessageText.
//
// Keeping this glue in runtime (which already imports both packages)
// preserves the layering: domain stays dependency-free, tg stays
// transport-only, and the V7 pipeline negotiates capability via a
// type assertion on the same Sender field tests already inject.
package runtime

import (
	"context"

	"github.com/prowendi/PulseGuard/internal/domain"
	"github.com/prowendi/PulseGuard/internal/tg"
)

// tgSenderAdapter wraps a *tg.Client so it satisfies both the legacy
// domain.Sender contract AND the new domain.SenderWithOpts. The worker
// type-asserts to SenderWithOpts at the call site; legacy fakes that
// only implement Sender keep working because the assertion is
// fall-through (no buttons → no opts call).
type tgSenderAdapter struct {
	client *tg.Client
}

// newTGSenderAdapter is a thin constructor so the wiring in run.go
// stays readable: `sender := newTGSenderAdapter(tg.New(...))`.
func newTGSenderAdapter(c *tg.Client) *tgSenderAdapter {
	return &tgSenderAdapter{client: c}
}

// Send satisfies domain.Sender by forwarding to the embedded client.
func (a *tgSenderAdapter) Send(ctx context.Context, botToken, chatID, parseMode, text string) (int64, error) {
	return a.client.Send(ctx, botToken, chatID, parseMode, text)
}

// SendWithOpts satisfies domain.SenderWithOpts. domain.PushButton →
// tg.InlineButton is a 1:1 field copy (separate types only because
// the layering forbids tg importing domain).
func (a *tgSenderAdapter) SendWithOpts(ctx context.Context, botToken, chatID, parseMode, text string, opts domain.SendOptions) (int64, error) {
	tgOpts := tg.SendOpts{}
	if n := len(opts.Buttons); n > 0 {
		tgOpts.Buttons = make([]tg.InlineButton, 0, n)
		for _, b := range opts.Buttons {
			tgOpts.Buttons = append(tgOpts.Buttons, tg.InlineButton{
				Text:     b.Text,
				Callback: b.Callback,
				URL:      b.URL,
			})
		}
	}
	return a.client.SendWithOpts(ctx, botToken, chatID, parseMode, text, tgOpts)
}

// EditMessage satisfies domain.SenderWithOpts. Direct pass-through;
// the *tg.Client.Edit signature is already (botToken, chatID,
// messageID, parseMode, text) so no reshape is needed.
func (a *tgSenderAdapter) EditMessage(ctx context.Context, botToken, chatID string, messageID int64, parseMode, text string) error {
	return a.client.Edit(ctx, botToken, chatID, messageID, parseMode, text)
}

// Compile-time conformance — when these break, an interface drift in
// internal/domain caught a wire-up regression here rather than in
// production.
var (
	_ domain.Sender         = (*tgSenderAdapter)(nil)
	_ domain.SenderWithOpts = (*tgSenderAdapter)(nil)
)
