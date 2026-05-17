package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/wendi/pulseguard/internal/condeval"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/render"
	"github.com/wendi/pulseguard/internal/tg"
)

// WorkerDeps groups every collaborator the worker needs to drain the
// outbox. All fields are interfaces so the worker is fakeable end-to-end.
type WorkerDeps struct {
	Outbox    domain.OutboxRepo
	Channels  domain.ChannelRepo
	Bots      domain.BotRepo
	Templates domain.TemplateRepo
	Logs      domain.LogRepo
	DLQ       domain.DeadLetterRepo
	RL        domain.RateLimiter
	Sender    domain.Sender
	Clock     domain.Clock
	// MessageThreads, when non-nil, drives the V7-2 editMessageText
	// state machine: payloads carrying an `_fingerprint` consult the
	// thread table on hot-path. nil-safe — when the dependency is
	// absent the worker degrades to vanilla Send so legacy fixtures
	// (and the simple unit tests that pre-date V7-2) keep working.
	MessageThreads domain.MessageThreadRepo
	// Silences, when non-nil, gates the outbound Send/Edit on a
	// prefix match against the tenant's active silence rules (V7-3).
	// When a payload's `_fingerprint` matches an active silence, the
	// outbox row is MarkSent + LogSilenced, and the Sender is never
	// called. nil-safe: tests without a silence dependency see no
	// behaviour change.
	Silences domain.SilenceRepo
	// Logger receives structured records for every claim/sent/retry/dead
	// branch. Nil is acceptable — a noop logger writing to io.Discard is
	// substituted so call sites never need to nil-check.
	Logger *slog.Logger
}

// noopLogger returns a slog.Logger that discards every record. Used when
// callers leave WorkerDeps.Logger unset so the tick path can always emit
// structured records unconditionally.
func noopLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// WorkerCfg captures the per-worker tunables.
type WorkerCfg struct {
	WorkerID     string
	PollInterval time.Duration
	MaxAttempts  int
	Backoff      Backoff
}

// Worker drains push_outbox according to the lifecycle described in spec
// §4.1. One worker = one goroutine. Multiple workers can run in parallel
// safely because OutboxRepo.ClaimNext is row-atomic.
type Worker struct {
	deps WorkerDeps
	cfg  WorkerCfg
}

// New constructs a Worker. The caller is responsible for ensuring the cfg
// values are sensible (Backoff.MaxAttempts > 0, PollInterval > 0).
func New(deps WorkerDeps, cfg WorkerCfg) *Worker {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = cfg.Backoff.MaxAttempts
	}
	if cfg.WorkerID == "" {
		cfg.WorkerID = "worker-1"
	}
	if deps.Logger == nil {
		deps.Logger = noopLogger()
	}
	return &Worker{deps: deps, cfg: cfg}
}

// Run blocks until ctx is cancelled, polling the outbox on every idle
// loop. Returns ctx.Err() once it exits.
func (w *Worker) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		didWork, err := w.tick(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			// Surface per-tick errors via the injected logger so the
			// transient SQL hiccup that prompted the swallow at least
			// shows up in production observability.
			w.deps.Logger.Warn("pipeline.worker tick error",
				"worker_id", w.cfg.WorkerID,
				"err", err.Error())
		}
		if didWork {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(w.cfg.PollInterval):
		}
	}
}

// tick processes at most one outbox row. Returns (didWork, err).
//
// didWork=true means a row was claimed and routed (whether sent, retried,
// or DLQ'd) and the caller should loop immediately to drain the queue.
// didWork=false means nothing was eligible — the caller should sleep.
func (w *Worker) tick(ctx context.Context) (bool, error) {
	now := w.deps.Clock.Now()
	item, err := w.deps.Outbox.ClaimNext(ctx, w.cfg.WorkerID, now)
	if err != nil {
		return false, fmt.Errorf("claim: %w", err)
	}
	if item == nil {
		return false, nil
	}
	w.deps.Logger.Info("pipeline.worker claimed",
		"worker_id", w.cfg.WorkerID,
		"outbox_id", item.ID,
		"channel_id", item.ChannelID,
		"attempts", item.Attempts)

	ch, err := w.deps.Channels.GetByID(ctx, item.TenantID, item.ChannelID)
	if err != nil {
		// Channel vanished (deleted while in flight). Treat as permanent.
		w.dlq(ctx, item, "", fmt.Sprintf("channel lookup: %s", err.Error()))
		return true, nil
	}
	if !ch.Enabled {
		w.dlq(ctx, item, "", "channel disabled")
		return true, nil
	}

	bot, err := w.deps.Bots.GetByID(ctx, item.TenantID, ch.BotID)
	if err != nil {
		w.dlq(ctx, item, "", fmt.Sprintf("bot lookup: %s", err.Error()))
		return true, nil
	}
	// Pick template: payload._template (set by Ingest when caller
	// requested a specific template) wins, else channel default.
	payload := decodePayload(item.PayloadJSON)
	templateID, pickErr := pickTemplateID(ch, payload)
	if pickErr != nil {
		w.dlq(ctx, item, "", pickErr.Error())
		return true, nil
	}
	if templateID == 0 {
		w.dlq(ctx, item, "", "channel has no template binding")
		return true, nil
	}
	tpl, err := w.deps.Templates.GetByID(ctx, item.TenantID, templateID)
	if err != nil {
		w.dlq(ctx, item, "", fmt.Sprintf("template lookup: %s", err.Error()))
		return true, nil
	}

	// Rate limit check before paying the render cost. Failure here is
	// soft: we re-queue with a 1s delay.
	allowed, err := w.deps.RL.Allow(ctx, ch.ID, ch.RatePerMin)
	if err != nil {
		// Treat unexpected rate-limit errors as transient; brief retry.
		w.markRetryOrDead(ctx, item, "", fmt.Errorf("rate-limit: %w", err))
		return true, nil
	}
	if !allowed {
		next := w.deps.Clock.Now().Add(time.Second)
		if err := w.deps.Outbox.MarkRetry(ctx, item.ID, next, "rate-limited"); err != nil {
			w.deps.Logger.Warn("pipeline.worker mark retry (rate-limited) failed",
				"worker_id", w.cfg.WorkerID,
				"outbox_id", item.ID,
				"channel_id", item.ChannelID,
				"err", err.Error())
		} else {
			w.deps.Logger.Info("pipeline.worker rate-limited; retry queued",
				"worker_id", w.cfg.WorkerID,
				"outbox_id", item.ID,
				"channel_id", item.ChannelID)
		}
		return true, nil
	}

	// Render — failure is always permanent (template bugs do not heal).
	text, err := render.Render(ctx, tpl, payload)
	if err != nil {
		w.deps.Logger.Warn("pipeline.worker render failed",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"err", err.Error())
		w.dlq(ctx, item, "", fmt.Sprintf("render: %s", err.Error()))
		return true, nil
	}

	parseMode := string(tpl.ParseMode)
	// V7-1: extract optional inline keyboard from payload._buttons.
	// The convention is "underscore prefix = pipeline directive, not
	// template data" — same shape as _template_id / _template. Buttons
	// are silently dropped when:
	//   - the sender does not implement SenderWithOpts (legacy fake in
	//     tests), so the fall-back to plain Send below preserves the
	//     buttonless behaviour every existing test expects;
	//   - the payload does not carry _buttons, so the buttons slice is
	//     nil and SendWithOpts behaves identically to Send.
	// Either path keeps the V7-1 change strictly additive for callers
	// that have not opted in.
	buttons := extractButtons(payload)

	// V7-2: collapse repeat alerts that share an `_fingerprint`. The
	// branch is opt-in by payload: callers that omit `_fingerprint`
	// retain the pre-V7 single-shot Send behaviour exactly. When the
	// fingerprint is present AND a message_threads row exists AND the
	// Sender implements EditMessage we rewrite the live Telegram message
	// in place. Everything else falls through to Send and (on success)
	// stamps a fresh thread row so the next push edits this one.
	fingerprint := extractFingerprint(payload)

	// V7-3: gate every outbound push on the tenant's active silence
	// rules. The check runs BEFORE the V7-2 edit lookup so a silenced
	// alert never edits its previous message — the operator silenced
	// it precisely because they want the chat quiet, not "quietly
	// rewritten". Failures here fall through (alert ships normally)
	// because a transient SQL hiccup must not stop a legitimate
	// notification.
	if fingerprint != "" && w.deps.Silences != nil {
		matched, sErr := w.deps.Silences.Match(ctx, item.TenantID, fingerprint, w.deps.Clock.Now())
		if sErr != nil {
			w.deps.Logger.Warn("pipeline.worker silence lookup failed; sending anyway",
				"worker_id", w.cfg.WorkerID,
				"outbox_id", item.ID,
				"channel_id", item.ChannelID,
				"fingerprint", fingerprint,
				"err", sErr.Error())
		} else if matched {
			w.markSilenced(ctx, item, text, fingerprint)
			return true, nil
		}
	}

	if fingerprint != "" && w.deps.MessageThreads != nil {
		if existing, lookupErr := w.deps.MessageThreads.GetByFingerprint(ctx, ch.ID, fingerprint); lookupErr == nil && existing != nil {
			if sw, ok := w.deps.Sender.(domain.SenderWithOpts); ok {
				if editErr := sw.EditMessage(ctx, bot.BotToken, ch.ChatID, existing.TGMessageID, parseMode, text); editErr == nil {
					w.markEdited(ctx, item, text, existing)
					return true, nil
				} else {
					// Edit failed — classify same as Send. The thread row
					// stays in place; a successful subsequent send below
					// will upsert it with the new tg_message_id so the next
					// edit targets the right message.
					return w.handleSendErr(ctx, item, text, editErr), nil
				}
			}
			// Sender lacks EditMessage capability (legacy fake). Fall
			// through to a normal send; the surrounding plumbing keeps
			// the duplicate noise the operator was hoping to avoid, but
			// the alert still lands.
			w.deps.Logger.Info("pipeline.worker edit fallback (sender does not implement SenderWithOpts)",
				"worker_id", w.cfg.WorkerID,
				"outbox_id", item.ID,
				"channel_id", item.ChannelID,
				"fingerprint", fingerprint)
		} else if lookupErr != nil && !errors.Is(lookupErr, domain.ErrNotFound) {
			// A SQL-level lookup error is non-fatal: log and continue with
			// a vanilla send so the alert still ships. Without this guard
			// a transient SQLite hiccup would DLQ legitimate pushes.
			w.deps.Logger.Warn("pipeline.worker message_thread lookup failed; falling back to send",
				"worker_id", w.cfg.WorkerID,
				"outbox_id", item.ID,
				"channel_id", item.ChannelID,
				"fingerprint", fingerprint,
				"err", lookupErr.Error())
		}
	}

	msgID, sendErr := w.dispatchSend(ctx, bot.BotToken, ch.ChatID, parseMode, text, buttons)
	if sendErr == nil {
		w.markSent(ctx, item, text, msgID)
		// V7-2: stamp the message_threads row so the next push with the
		// same fingerprint edits THIS message. Failures here are
		// non-fatal — the send already happened; worst case the next
		// payload sends a fresh message rather than collapsing.
		if fingerprint != "" && w.deps.MessageThreads != nil {
			thread := &domain.MessageThread{
				ChannelID:   ch.ID,
				TenantID:    item.TenantID,
				Fingerprint: fingerprint,
				ChatID:      ch.ChatID,
				TGMessageID: msgID,
			}
			if upErr := w.deps.MessageThreads.Upsert(ctx, thread); upErr != nil {
				w.deps.Logger.Warn("pipeline.worker upsert message_thread failed",
					"worker_id", w.cfg.WorkerID,
					"outbox_id", item.ID,
					"channel_id", item.ChannelID,
					"fingerprint", fingerprint,
					"err", upErr.Error())
			}
		}
		return true, nil
	}

	if !w.handleSendErr(ctx, item, text, sendErr) {
		// handleSendErr returns false only when no terminal action ran —
		// keep the original behaviour: report didWork=true so the worker
		// loop continues. (All current branches return true; the guard
		// is defensive against future refactors.)
		return true, nil
	}
	return true, nil
}

// handleSendErr classifies a Send or Edit failure and dispatches to the
// transient-retry or DLQ branch. Returns true when the row's terminal
// state (sent/retry/dead) was updated — every current path returns true
// so the caller always reports didWork.
func (w *Worker) handleSendErr(ctx context.Context, item *domain.PushOutbox, rendered string, sendErr error) bool {
	if ae, ok := tg.AsAPIError(sendErr); ok {
		switch ae.Class {
		case tg.Transient:
			w.handleTransient(ctx, item, rendered, ae)
		case tg.PermanentClient, tg.PermanentServer:
			w.dlq(ctx, item, rendered, sendErr.Error())
		default:
			w.markRetryOrDead(ctx, item, rendered, sendErr)
		}
	} else {
		w.markRetryOrDead(ctx, item, rendered, sendErr)
	}
	return true
}

func (w *Worker) handleTransient(ctx context.Context, item *domain.PushOutbox, rendered string, ae *tg.APIError) {
	// Have we exhausted attempts? item.Attempts already includes this attempt
	// because ClaimNext incremented it before handing us the row.
	if item.Attempts >= w.cfg.MaxAttempts {
		w.dlq(ctx, item, rendered, ae.Error())
		return
	}
	delay, isFinal := w.cfg.Backoff.NextDelay(item.Attempts)
	// RetryAfter from Telegram overrides our schedule when present.
	if ae.RetryAfter > 0 {
		delay = ae.RetryAfter
		isFinal = false
	}
	if isFinal {
		w.dlq(ctx, item, rendered, ae.Error())
		return
	}
	next := w.deps.Clock.Now().Add(delay)
	if err := w.deps.Outbox.MarkRetry(ctx, item.ID, next, ae.Error()); err != nil {
		w.deps.Logger.Warn("pipeline.worker mark retry failed",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"attempts", item.Attempts,
			"err", err.Error())
		return
	}
	w.deps.Logger.Info("pipeline.worker retry scheduled",
		"worker_id", w.cfg.WorkerID,
		"outbox_id", item.ID,
		"channel_id", item.ChannelID,
		"attempts", item.Attempts,
		"next_attempt_at", next.UTC().Format(time.RFC3339),
		"reason", ae.Error())
}

func (w *Worker) markRetryOrDead(ctx context.Context, item *domain.PushOutbox, rendered string, sendErr error) {
	if item.Attempts >= w.cfg.MaxAttempts {
		w.dlq(ctx, item, rendered, sendErr.Error())
		return
	}
	delay, isFinal := w.cfg.Backoff.NextDelay(item.Attempts)
	if isFinal {
		w.dlq(ctx, item, rendered, sendErr.Error())
		return
	}
	next := w.deps.Clock.Now().Add(delay)
	if err := w.deps.Outbox.MarkRetry(ctx, item.ID, next, sendErr.Error()); err != nil {
		w.deps.Logger.Warn("pipeline.worker mark retry failed",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"attempts", item.Attempts,
			"err", err.Error())
		return
	}
	w.deps.Logger.Info("pipeline.worker retry scheduled",
		"worker_id", w.cfg.WorkerID,
		"outbox_id", item.ID,
		"channel_id", item.ChannelID,
		"attempts", item.Attempts,
		"next_attempt_at", next.UTC().Format(time.RFC3339),
		"reason", sendErr.Error())
}

// markSilenced is the V7-3 terminal path: the push matched an active
// silence rule, so we MarkSent the outbox row (work is done) and log
// the terminal event with status=silenced for the audit trail. The
// Sender is NEVER called and no message_thread bookkeeping happens —
// the silence is total.
//
// Render failures cannot reach this path (silence is checked AFTER
// render succeeds) so the LogSilenced row always carries a useful
// rendered text for the operator's later audit.
func (w *Worker) markSilenced(ctx context.Context, item *domain.PushOutbox, rendered, fingerprint string) {
	log := &domain.PushLog{
		OutboxID:     &item.ID,
		ChannelID:    item.ChannelID,
		TenantID:     item.TenantID,
		PayloadJSON:  item.PayloadJSON,
		RenderedText: rendered,
		Status:       domain.LogSilenced,
		Attempts:     item.Attempts,
	}
	if err := w.deps.Logs.Insert(ctx, log); err != nil {
		w.deps.Logger.Warn("pipeline.worker insert silenced log failed",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"err", err.Error())
	}
	if err := w.deps.Outbox.MarkSent(ctx, item.ID, w.deps.Clock.Now()); err != nil {
		w.deps.Logger.Error("pipeline.worker mark sent (after silence) failed",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"err", err.Error())
		return
	}
	w.deps.Logger.Info("pipeline.worker silenced",
		"worker_id", w.cfg.WorkerID,
		"outbox_id", item.ID,
		"channel_id", item.ChannelID,
		"attempts", item.Attempts,
		"fingerprint", fingerprint)
}

// markEdited is the V7-2 sibling of markSent. The push collapsed into
// an existing message_thread, so we MarkSent the outbox row (the work
// is done) and log the terminal event with status=edited so the audit
// trail distinguishes a fresh send from an in-place update. The thread
// row's tg_message_id stays as-is (the edit keeps the original message
// alive) so subsequent pushes continue editing the same Telegram entry.
func (w *Worker) markEdited(ctx context.Context, item *domain.PushOutbox, rendered string, thread *domain.MessageThread) {
	mid := thread.TGMessageID
	log := &domain.PushLog{
		OutboxID:     &item.ID,
		ChannelID:    item.ChannelID,
		TenantID:     item.TenantID,
		PayloadJSON:  item.PayloadJSON,
		RenderedText: rendered,
		TGMessageID:  &mid,
		Status:       domain.LogEdited,
		Attempts:     item.Attempts,
	}
	if err := w.deps.Logs.Insert(ctx, log); err != nil {
		w.deps.Logger.Warn("pipeline.worker insert edited log failed",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"err", err.Error())
	}
	// Bump the thread's updated_at so /silence_list / future UI shows
	// freshness without changing the underlying message_id.
	if err := w.deps.MessageThreads.Upsert(ctx, thread); err != nil {
		w.deps.Logger.Warn("pipeline.worker message_thread bump failed",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"err", err.Error())
	}
	if err := w.deps.Outbox.MarkSent(ctx, item.ID, w.deps.Clock.Now()); err != nil {
		w.deps.Logger.Error("pipeline.worker mark sent (after edit) failed",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"tg_message_id", thread.TGMessageID,
			"err", err.Error())
		return
	}
	w.deps.Logger.Info("pipeline.worker edited",
		"worker_id", w.cfg.WorkerID,
		"outbox_id", item.ID,
		"channel_id", item.ChannelID,
		"attempts", item.Attempts,
		"tg_message_id", thread.TGMessageID,
		"fingerprint", thread.Fingerprint)
}

func (w *Worker) markSent(ctx context.Context, item *domain.PushOutbox, rendered string, msgID int64) {
	mid := msgID
	log := &domain.PushLog{
		OutboxID:     &item.ID,
		ChannelID:    item.ChannelID,
		TenantID:     item.TenantID,
		PayloadJSON:  item.PayloadJSON,
		RenderedText: rendered,
		TGMessageID:  &mid,
		Status:       domain.LogSent,
		Attempts:     item.Attempts,
	}
	if err := w.deps.Logs.Insert(ctx, log); err != nil {
		w.deps.Logger.Warn("pipeline.worker insert sent log failed",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"err", err.Error())
	}
	if err := w.deps.Outbox.MarkSent(ctx, item.ID, w.deps.Clock.Now()); err != nil {
		// MarkSent failure is the worst-case observability gap: the row
		// stays in_flight and the cleanup loop will reclaim it as retry,
		// leading to a duplicate send. Promote to Error so on-call notices.
		w.deps.Logger.Error("pipeline.worker mark sent failed (risk of duplicate)",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"tg_message_id", msgID,
			"err", err.Error())
		return
	}
	w.deps.Logger.Info("pipeline.worker sent",
		"worker_id", w.cfg.WorkerID,
		"outbox_id", item.ID,
		"channel_id", item.ChannelID,
		"attempts", item.Attempts,
		"tg_message_id", msgID)
}

func (w *Worker) dlq(ctx context.Context, item *domain.PushOutbox, rendered string, reason string) {
	var renderedPtr *string
	if rendered != "" {
		r := rendered
		renderedPtr = &r
	}
	errStr := reason
	dl := &domain.DeadLetter{
		OutboxID:     item.ID,
		ChannelID:    item.ChannelID,
		TenantID:     item.TenantID,
		PayloadJSON:  item.PayloadJSON,
		RenderedText: renderedPtr,
		LastError:    reason,
		Attempts:     item.Attempts,
	}
	if err := w.deps.DLQ.Insert(ctx, dl); err != nil {
		// Losing a DLQ row erases audit history; surface as Error.
		w.deps.Logger.Error("pipeline.worker DLQ insert failed (audit gap)",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"err", err.Error())
	}
	log := &domain.PushLog{
		OutboxID:     &item.ID,
		ChannelID:    item.ChannelID,
		TenantID:     item.TenantID,
		PayloadJSON:  item.PayloadJSON,
		RenderedText: rendered,
		Status:       domain.LogDead,
		Error:        &errStr,
		Attempts:     item.Attempts,
	}
	if err := w.deps.Logs.Insert(ctx, log); err != nil {
		w.deps.Logger.Warn("pipeline.worker insert dead log failed",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"err", err.Error())
	}
	if err := w.deps.Outbox.MarkDead(ctx, item.ID, reason); err != nil {
		w.deps.Logger.Error("pipeline.worker mark dead failed (row stuck in_flight)",
			"worker_id", w.cfg.WorkerID,
			"outbox_id", item.ID,
			"channel_id", item.ChannelID,
			"err", err.Error())
		return
	}
	w.deps.Logger.Warn("pipeline.worker dead",
		"worker_id", w.cfg.WorkerID,
		"outbox_id", item.ID,
		"channel_id", item.ChannelID,
		"attempts", item.Attempts,
		"reason", reason)
}

// decodePayload turns the outbox JSON payload back into a map. Malformed
// payloads degrade to an empty map so template rendering still runs
// (and either succeeds with sane defaults or fails permanently itself).
func decodePayload(raw string) map[string]any {
	if raw == "" {
		return map[string]any{}
	}
	var m map[string]any
	if err := unmarshal(raw, &m); err != nil || m == nil {
		return map[string]any{}
	}
	return m
}

// dispatchSend routes the outbound message through the buttons-aware
// SenderWithOpts when the injected Sender implements it; otherwise it
// falls back to the legacy Sender.Send so V7-1 stays a pure
// extension. The fallback path also runs when buttons is empty — the
// resulting Telegram body is byte-identical to the pre-V7 behaviour
// so the existing happy-path tests keep their evidence value.
func (w *Worker) dispatchSend(ctx context.Context, botToken, chatID, parseMode, text string, buttons []domain.PushButton) (int64, error) {
	if len(buttons) == 0 {
		return w.deps.Sender.Send(ctx, botToken, chatID, parseMode, text)
	}
	if sw, ok := w.deps.Sender.(domain.SenderWithOpts); ok {
		return sw.SendWithOpts(ctx, botToken, chatID, parseMode, text, domain.SendOptions{Buttons: buttons})
	}
	// Sender does not understand buttons — log once at info so the
	// operator notices the dropped markup, then fall back to a
	// vanilla send so the alert still lands.
	w.deps.Logger.Info("pipeline.worker buttons dropped (sender does not implement SenderWithOpts)",
		"worker_id", w.cfg.WorkerID,
		"button_count", len(buttons))
	return w.deps.Sender.Send(ctx, botToken, chatID, parseMode, text)
}

// extractFingerprint pulls the optional `_fingerprint` directive from
// the decoded payload. Empty/missing/non-string values collapse to ""
// which the worker uses as the "no V7-2 routing" sentinel: the row
// goes through the legacy single-shot Send path with no thread bookkeeping.
//
// Whitespace is trimmed so callers cannot accidentally split message
// threads by typing a trailing space; "  db01-cpu  " and "db01-cpu"
// must collapse together.
func extractFingerprint(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	raw, ok := payload["_fingerprint"]
	if !ok {
		return ""
	}
	s, ok := raw.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(s)
}

// extractButtons projects payload["_buttons"] into a []domain.PushButton
// the worker can hand to SendWithOpts. The wire format is a JSON array
// of objects:
//
//	"_buttons": [
//	  {"text": "ACK", "callback": "ack:db01-cpu"},
//	  {"text": "Runbook", "url": "https://example.com/rb"}
//	]
//
// Malformed entries are silently skipped — a typo must not crash the
// pipeline; the operator will notice the missing button in the
// rendered Telegram message. Returns nil when no buttons are present.
func extractButtons(payload map[string]any) []domain.PushButton {
	raw, ok := payload["_buttons"]
	if !ok || raw == nil {
		return nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil
	}
	out := make([]domain.PushButton, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		btn := domain.PushButton{}
		if v, ok := obj["text"].(string); ok {
			btn.Text = v
		}
		if v, ok := obj["callback"].(string); ok {
			btn.Callback = v
		}
		if v, ok := obj["url"].(string); ok {
			btn.URL = v
		}
		if btn.Text == "" || (btn.Callback == "" && btn.URL == "") {
			continue
		}
		out = append(out, btn)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// pickTemplateID resolves which template the worker should render with.
// Precedence:
//  1. payload["_template_id"] (numeric) — set by Ingest when the push
//     API caller passed ?template=<name>, resolved against the channel's
//     bound templates at ingest time. If the requested ID is NOT bound
//     to the channel an error is returned and the worker DLQs the row;
//     silently falling back to the default would route the payload to
//     a template the caller explicitly did not select.
//  2. payload["_template"] (string) — case-insensitive name match
//     against the channel's bound templates (fallback for direct
//     enqueue paths that did not pre-resolve).
//  3. condition match — walk channel.Templates in SortOrder ASC and
//     return the first binding whose non-empty Condition evaluates
//     true against the payload (internal/condeval). Empty conditions
//     are skipped here; they participate only as default-eligible
//     candidates (rule 4).
//  4. channel default template (channel_templates.is_default = 1).
//
// A malformed condition string is treated as a non-match and ignored
// for routing purposes — bad input must not crash the worker nor send
// the payload through an unintended template. The operator surfaces
// the typo through the UI (and condition fields are validated on
// write), not via runtime detonation.
//
// Returns 0 when no template can be selected; the worker DLQs the row.
func pickTemplateID(ch *domain.Channel, payload map[string]any) (int64, error) {
	if ch == nil {
		return 0, nil
	}
	if v, ok := payload["_template_id"]; ok {
		var requested int64
		switch t := v.(type) {
		case float64:
			requested = int64(t)
		case int64:
			requested = t
		default:
			// Unknown numeric type — ignore and fall through to default
			// (decodePayload via encoding/json only yields float64 for
			// JSON numbers, but keep the int64 case for direct map
			// construction tests).
			return ch.DefaultTemplateID(), nil
		}
		if requested == 0 {
			return ch.DefaultTemplateID(), nil
		}
		if !ch.HasTemplate(requested) {
			return 0, fmt.Errorf("requested template_id %d not bound to channel %d", requested, ch.ID)
		}
		return requested, nil
	}
	if v, ok := payload["_template"].(string); ok && v != "" {
		// We do not have template names cached on the channel; this
		// path is only reachable when an external system enqueues with
		// a name. Worker callers (web/push_api.go) always pre-resolve
		// to _template_id so this is best-effort only.
		_ = v
	}
	// Condition-based auto-routing. Bindings are pre-sorted (loadBindings
	// orders by sort_order ASC, template_id ASC) so the iteration order
	// is deterministic and matches operator intent.
	for _, ct := range ch.Templates {
		if ct == nil || ct.Condition == "" {
			continue
		}
		cond, err := condeval.Parse(ct.Condition)
		if err != nil {
			// Malformed condition: skip and continue. Worth surfacing
			// via the audit log eventually, but never crash the worker.
			continue
		}
		if cond.Match(payload) {
			return ct.TemplateID, nil
		}
	}
	return ch.DefaultTemplateID(), nil
}
