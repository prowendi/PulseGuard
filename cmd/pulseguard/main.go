// Command pulseguard is the single-binary entrypoint. It loads the YAML
// config (with PULSEGUARD_* env overrides), configures slog, sets up
// SIGINT/SIGTERM cancellation, and forwards to runtime.Run, which owns
// the full wire-up. Everything testable lives in internal/runtime.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/wendi/pulseguard/internal/config"
	"github.com/wendi/pulseguard/internal/logging"
	"github.com/wendi/pulseguard/internal/runtime"
)

func main() {
	configPath := flag.String("config", "config.yaml", "Path to YAML config file")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	logger := logging.New(cfg.Logging.Level, cfg.Logging.Format)
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := runtime.Run(ctx, cfg, logger); err != nil {
		logger.Error("pulseguard exited with error", "error", err)
		os.Exit(1)
	}
}
