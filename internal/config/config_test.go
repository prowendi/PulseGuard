package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("server:\n  listen_addr: \":9999\"\nworker:\n  count: 8\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ListenAddr != ":9999" {
		t.Fatalf("ListenAddr = %q", cfg.Server.ListenAddr)
	}
	if cfg.Worker.Count != 8 {
		t.Fatalf("Worker.Count = %d", cfg.Worker.Count)
	}
}

func TestEnvOverride(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("server:\n  listen_addr: \":8080\"\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("PULSEGUARD_SERVER_LISTEN_ADDR", ":7777")
	t.Setenv("PULSEGUARD_WORKER_COUNT", "16")
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ListenAddr != ":7777" {
		t.Fatalf("env override addr = %q", cfg.Server.ListenAddr)
	}
	if cfg.Worker.Count != 16 {
		t.Fatalf("env override count = %d", cfg.Worker.Count)
	}
}

func TestDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte("{}"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ReadTimeout.Std() != 10*time.Second {
		t.Fatalf("default ReadTimeout = %v", cfg.Server.ReadTimeout.Std())
	}
	if cfg.Worker.MaxAttempts != 6 {
		t.Fatalf("default MaxAttempts = %d", cfg.Worker.MaxAttempts)
	}
	if len(cfg.Worker.BackoffSchedule) != 6 {
		t.Fatalf("default BackoffSchedule len = %d", len(cfg.Worker.BackoffSchedule))
	}
	if cfg.Worker.BackoffSchedule[0].Std() != time.Second {
		t.Fatalf("default BackoffSchedule[0] = %v", cfg.Worker.BackoffSchedule[0].Std())
	}
}

func TestYAMLDurationFromString(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	body := "server:\n  read_timeout: 7s\nworker:\n  backoff_schedule: [2s, 4s, 8s]\n"
	if err := os.WriteFile(p, []byte(body), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ReadTimeout.Std() != 7*time.Second {
		t.Fatalf("parsed ReadTimeout = %v", cfg.Server.ReadTimeout.Std())
	}
	if len(cfg.Worker.BackoffSchedule) != 3 {
		t.Fatalf("BackoffSchedule len = %d", len(cfg.Worker.BackoffSchedule))
	}
	if cfg.Worker.BackoffSchedule[2].Std() != 8*time.Second {
		t.Fatalf("BackoffSchedule[2] = %v", cfg.Worker.BackoffSchedule[2].Std())
	}
}
