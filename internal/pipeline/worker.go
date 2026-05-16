package pipeline

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

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
	tpl, err := w.deps.Templates.GetByID(ctx, item.TenantID, ch.TemplateID)
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
	payload := decodePayload(item.PayloadJSON)
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
	msgID, sendErr := w.deps.Sender.Send(ctx, bot.BotToken, ch.ChatID, parseMode, text)
	if sendErr == nil {
		w.markSent(ctx, item, text, msgID)
		return true, nil
	}

	// Classify failure.
	if ae, ok := tg.AsAPIError(sendErr); ok {
		switch ae.Class {
		case tg.Transient:
			w.handleTransient(ctx, item, text, ae)
		case tg.PermanentClient, tg.PermanentServer:
			w.dlq(ctx, item, text, sendErr.Error())
		default:
			// Unknown class -> conservative retry.
			w.markRetryOrDead(ctx, item, text, sendErr)
		}
	} else {
		// Untyped error from sender -> conservative retry.
		w.markRetryOrDead(ctx, item, text, sendErr)
	}
	return true, nil
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
