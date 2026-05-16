// Package runtime hosts the wire-up of every PulseGuard component in a
// way that is reachable from tests. cmd/pulseguard/main.go is a thin
// shim that parses flags and forwards to Run.
//
// Run is split from RunWithDeps so integration tests can substitute a
// fake Telegram sender without reaching out to the network.
package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/wendi/pulseguard/internal/auth"
	"github.com/wendi/pulseguard/internal/bootstrap"
	"github.com/wendi/pulseguard/internal/cmdrun"
	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/pipeline"
	"github.com/wendi/pulseguard/internal/platform"
	"github.com/wendi/pulseguard/internal/platform/telegram"
	"github.com/wendi/pulseguard/internal/scripting"
	"github.com/wendi/pulseguard/internal/store"
	"github.com/wendi/pulseguard/internal/tg"
	"github.com/wendi/pulseguard/internal/web"
)

// Overrides lets callers (chiefly tests) replace selected dependencies
// without forking Run. Any field left zero/nil falls back to the real
// production implementation.
type Overrides struct {
	// Sender, when non-nil, replaces tg.New() — used by tests to stub
	// out network calls.
	Sender domain.Sender
	// Clock, when non-nil, replaces domain.RealClock(). Worker poll
	// intervals still come from the real time package; the clock is
	// only used for timestamp generation inside repos & services.
	Clock domain.Clock
	// ListenerCh, when non-nil, receives the actual *net.TCPAddr the
	// HTTP server bound to. Set by RunWithDeps before ListenAndServe
	// returns. Tests use this to discover the dynamically-assigned
	// port when ListenAddr is ":0".
	ListenerCh chan<- net.Addr
	// ReadyCh, when non-nil, is closed once HTTP server, workers, and
	// cleanup loops have all been spawned. Tests rely on this signal
	// before issuing requests.
	ReadyCh chan<- struct{}

	// BotListenerFactories, when non-empty, REPLACES the default set
	// of bot-listener factories (one per platform). Tests pass a fake
	// factory pointing at an httptest Telegram backend so create-bot
	// → /start → reply-with-chat-id can be asserted without touching
	// api.telegram.org.
	BotListenerFactories []platform.Factory
}

// Run is the production entrypoint. It instantiates a real
// tg.Client + RealClock and blocks until ctx is cancelled or a
// long-running task fails.
func Run(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	return RunWithDeps(ctx, cfg, logger, Overrides{})
}

// RunWithDeps is Run with selected components swappable via Overrides.
// It owns the entire process lifecycle: DB open + migrate + reclaim,
// repo construction, bootstrap, HTTP server, worker pool, cleanup
// loop, graceful shutdown, and WAL checkpoint on the way out.
func RunWithDeps(ctx context.Context, cfg *config.Config, logger *slog.Logger, ov Overrides) error {
	if cfg == nil {
		return errors.New("runtime: cfg is nil")
	}
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	clock := ov.Clock
	if clock == nil {
		clock = domain.RealClock()
	}

	// ── 1. DB: open + pragmas, migrate, reclaim in-flight outbox rows.
	db, err := store.Open(cfg.Database)
	if err != nil {
		return fmt.Errorf("runtime: open db: %w", err)
	}
	defer closeDB(db, logger)

	if err := store.Migrate(ctx, db, clock); err != nil {
		return fmt.Errorf("runtime: migrate: %w", err)
	}

	// Reclaim outbox rows that a prior process left in 'in_flight'.
	// Use the configured cutoff (default 60s) so we do not clobber rows
	// being processed by a peer in a multi-host setup (not supported in
	// MVP, but the cutoff makes the behaviour conservative).
	reclaimCutoff := clock.Now().Add(-cfg.Worker.InflightReclaimAfter.Std())
	if n, err := store.NewOutboxRepo(db, clock).ReclaimInFlight(ctx, reclaimCutoff); err != nil {
		logger.Warn("runtime: reclaim in-flight outbox failed", "error", err.Error())
	} else if n > 0 {
		logger.Info("runtime: reclaimed in-flight outbox rows", "count", n)
	}

	// ── 2. Cipher (AES-GCM, master_key_b64 from config).
	cipher, err := store.NewCipher(cfg.Security.MasterKeyB64)
	if err != nil {
		// Surface the weak-key error with a precise operator message
		// before the wrapped error bubbles up to main and exits non-zero.
		if errors.Is(err, store.ErrWeakMasterKey) {
			logger.Error("runtime: refusing to start with example master_key",
				"hint", "run `openssl rand -base64 32` and set PULSEGUARD_SECURITY_MASTER_KEY_B64")
		}
		return fmt.Errorf("runtime: cipher: %w", err)
	}

	// ── 3. Repos.
	tenants := store.NewTenantRepo(db, clock)
	invites := store.NewInviteRepo(db, clock)
	sessions := store.NewSessionRepo(db, clock)
	bots := store.NewBotRepo(db, clock, cipher)
	templates := store.NewTemplateRepo(db, clock)
	channels := store.NewChannelRepo(db, clock)
	outbox := store.NewOutboxRepo(db, clock)
	logs := store.NewLogRepo(db, clock)
	dlq := store.NewDeadLetterRepo(db, clock)
	dedupRepo := store.NewDedupRepo(db)
	rl := store.NewRateLimitRepo(db, clock)
	commands := store.NewCommandRepo(db, clock)
	subscribers := store.NewSubscriberRepo(db, clock)

	// ── 4. Sender (real TG client, unless overridden by tests).
	var sender domain.Sender = tg.New(cfg.Telegram)
	if ov.Sender != nil {
		sender = ov.Sender
	}

	// ── 5. Services.
	authSvc := auth.New(db, tenants, invites, sessions, cfg.Security, clock)
	dedup := pipeline.NewDedup(dedupRepo, clock)
	ingest := pipeline.NewIngestor(outbox, dedup, clock)

	// ── 6. Worker pool config (backoff schedule from yaml, with a
	// safe fallback).
	backoffSchedule := make([]time.Duration, 0, len(cfg.Worker.BackoffSchedule))
	for _, d := range cfg.Worker.BackoffSchedule {
		backoffSchedule = append(backoffSchedule, d.Std())
	}
	if len(backoffSchedule) == 0 {
		backoffSchedule = pipeline.DefaultBackoff().Schedule
	}
	backoff := pipeline.Backoff{
		Schedule:    backoffSchedule,
		MaxAttempts: cfg.Worker.MaxAttempts,
	}
	if backoff.MaxAttempts <= 0 {
		backoff.MaxAttempts = pipeline.DefaultBackoff().MaxAttempts
	}

	pid := os.Getpid()
	workerCount := cfg.Worker.Count
	if workerCount <= 0 {
		workerCount = 4
	}
	workers := make([]*pipeline.Worker, 0, workerCount)
	for i := 0; i < workerCount; i++ {
		w := pipeline.New(pipeline.WorkerDeps{
			Outbox:    outbox,
			Channels:  channels,
			Bots:      bots,
			Templates: templates,
			Logs:      logs,
			DLQ:       dlq,
			RL:        rl,
			Sender:    sender,
			Clock:     clock,
			Logger:    logger,
		}, pipeline.WorkerCfg{
			WorkerID:     fmt.Sprintf("w-%d-%d", pid, i),
			PollInterval: cfg.Worker.PollInterval.Std(),
			MaxAttempts:  backoff.MaxAttempts,
			Backoff:      backoff,
		})
		workers = append(workers, w)
	}

	// ── 7. Cleanup loop.
	cleanup := pipeline.NewCleanup(pipeline.CleanupDeps{
		Logs:     logs,
		Dedup:    dedupRepo,
		Sessions: sessions,
		Outbox:   outbox,
		Clock:    clock,
	}, pipeline.CleanupCfg{
		LogKeepDays:           cfg.Cleanup.PushLogsKeepDays,
		DedupSweepInterval:    cfg.Cleanup.DedupKeysSweepInterval.Std(),
		SessionsSweepInterval: cfg.Cleanup.SessionsSweepInterval.Std(),
		InflightReclaimAfter:  cfg.Worker.InflightReclaimAfter.Std(),
	})

	// ── 8. Bootstrap admin (fails loud on empty DB without creds).
	if err := bootstrap.EnsureInitialAdmin(ctx,
		bootstrap.BootstrapRepos{Tenants: tenants},
		cfg.Bootstrap, cfg.Security, clock,
	); err != nil {
		return fmt.Errorf("runtime: bootstrap: %w", err)
	}

	// ── 8b. Bot listener manager. Tests may inject a fake factory
	// (e.g. pointing at an httptest Telegram backend). In production
	// we wire in the real Telegram factory using the same api_base /
	// http_timeout as the outbound Sender so behaviour is uniform.
	//
	// The Telegram factory also accepts a CommandDispatcher built on
	// top of the Starlark Executor + commands/subscribers repos so
	// inbound `/<name>` messages route to per-tenant scripts.
	scriptHTTP := scripting.NewHTTPClient()
	scriptExec := &scripting.Executor{HTTP: scriptHTTP}
	dispatcher := cmdrun.New(commands, scriptExec, subscribers)

	listenerFactories := ov.BotListenerFactories
	if len(listenerFactories) == 0 {
		tgTimeout := cfg.Telegram.HTTPTimeout.Std()
		if tgTimeout <= 0 {
			tgTimeout = 30 * time.Second
		}
		listenerFactories = []platform.Factory{
			telegram.NewFactory(telegram.FactoryOptions{
				APIBase:    cfg.Telegram.APIBase,
				HTTP:       &http.Client{Timeout: tgTimeout},
				Logger:     logger,
				Dispatcher: dispatcher,
			}),
		}
	}
	listenerMgr := platform.NewManager(logger, listenerFactories...)

	// Boot a listener for every bot already in the DB so a process
	// restart resumes onboarding loops without operator action.
	if existing, err := bots.ListAll(ctx); err != nil {
		logger.Warn("runtime: list bots failed; listeners deferred to CRUD", "err", err.Error())
	} else {
		for _, b := range existing {
			if err := listenerMgr.Start(ctx, b); err != nil {
				logger.Warn("runtime: bot listener start failed",
					"bot_id", b.ID,
					"tenant_id", b.TenantID,
					"platform", b.Platform,
					"err", err.Error())
			}
		}
	}

	// ── 9. HTTP handler.
	handler := web.NewServer(web.Deps{
		Cfg:          cfg,
		Logger:       logger,
		Tenants:      tenants,
		Invites:      invites,
		Sessions:     sessions,
		Bots:         bots,
		Templates:    templates,
		Channels:     channels,
		Outbox:       outbox,
		Logs:         logs,
		DLQ:          dlq,
		RL:           rl,
		Commands:     commands,
		Subscribers:  subscribers,
		ScriptExec:   scriptExec,
		Cipher:       cipher,
		Auth:         authSvc,
		Ingest:       ingest,
		TG:           sender,
		Clock:        clock,
		BotListeners: listenerMgr,
	})

	// ── 10. Start HTTP listener on configured addr (":0" auto-port).
	ln, err := net.Listen("tcp", cfg.Server.ListenAddr)
	if err != nil {
		return fmt.Errorf("runtime: listen %s: %w", cfg.Server.ListenAddr, err)
	}
	if ov.ListenerCh != nil {
		ov.ListenerCh <- ln.Addr()
	}
	srv := &http.Server{
		Handler:      handler,
		ReadTimeout:  cfg.Server.ReadTimeout.Std(),
		WriteTimeout: cfg.Server.WriteTimeout.Std(),
	}

	// ── 11. Launch goroutines, fan-in their errors via errCh.
	errCh := make(chan error, len(workers)+2)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("runtime: http server listening", "addr", ln.Addr().String())
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("http server: %w", err)
		}
	}()

	for _, w := range workers {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				errCh <- fmt.Errorf("worker: %w", err)
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := cleanup.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errCh <- fmt.Errorf("cleanup: %w", err)
		}
	}()

	if ov.ReadyCh != nil {
		close(ov.ReadyCh)
	}

	// ── 12. Wait for ctx cancel OR any goroutine to surface an error.
	var runErr error
	select {
	case <-ctx.Done():
		logger.Info("runtime: shutdown signal received")
	case err := <-errCh:
		runErr = err
		logger.Error("runtime: goroutine error, initiating shutdown", "error", err.Error())
	}

	// ── 13. Graceful shutdown of HTTP server.
	shutTimeout := cfg.Server.ShutdownTimeout.Std()
	if shutTimeout <= 0 {
		shutTimeout = 15 * time.Second
	}
	shutCtx, cancel := context.WithTimeout(context.Background(), shutTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		logger.Warn("runtime: http server shutdown error", "error", err.Error())
	}

	// Tear down bot listeners before waiting on the WG — each listener
	// drains its long-poll via its own ctx cancel; Shutdown blocks
	// until they've all returned so the DB close below stays safe.
	listenerMgr.Shutdown()

	// Workers + cleanup observe ctx.Done() and exit. We then wait for
	// all goroutines so the DB close below is safe.
	wg.Wait()

	// Drain any remaining errors so we report all of them (the first
	// one wins for the return value).
	close(errCh)
	for err := range errCh {
		if runErr == nil {
			runErr = err
		} else {
			logger.Warn("runtime: additional shutdown error", "error", err.Error())
		}
	}

	// WAL checkpoint before close: best-effort, never fatal.
	if _, err := db.ExecContext(context.Background(), `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		logger.Warn("runtime: wal_checkpoint failed", "error", err.Error())
	}

	logger.Info("runtime: exited cleanly")
	return runErr
}

func closeDB(db *sql.DB, logger *slog.Logger) {
	if err := db.Close(); err != nil {
		logger.Warn("runtime: db close error", "error", err.Error())
	}
}
