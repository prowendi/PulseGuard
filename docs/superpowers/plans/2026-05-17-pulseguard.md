# PulseGuard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build a single-binary Go+SQLite SaaS alerting platform that ingests JSON webhook alerts and pushes them to Telegram via user-supplied bots, with retry/DLQ/rate-limit/dedup/history.

**Architecture:** chi-based HTTP server (HTMX+API) + Outbox/Worker async push pipeline, embedded into one Go binary backed by SQLite WAL. Tenant-isolated multi-tenant SaaS, invite-code registration, single static binary deployment.

**Tech Stack:** Go ≥1.22, `modernc.org/sqlite` (pure-Go, no cgo), `github.com/go-chi/chi/v5`, `github.com/go-chi/httprate`, `golang.org/x/crypto/bcrypt`, `crypto/aes`+`cipher.GCM`, `log/slog`, `htmx.org` v1.9+, `gopkg.in/yaml.v3`.

**Spec reference:** `docs/superpowers/specs/2026-05-17-pulseguard-design.md`

---

## Task 1: Bootstrap project skeleton

**Files:**
- Create: `E:\a2026\PulseGuard\go.mod`
- Create: `E:\a2026\PulseGuard\.gitignore`
- Create: `E:\a2026\PulseGuard\cmd\pulseguard\main.go`
- Create: `E:\a2026\PulseGuard\Makefile`

- [ ] **Step 1: Init module and git**

```bash
cd /e/a2026/PulseGuard
go mod init github.com/wendi/pulseguard
git init
```

- [ ] **Step 2: Write `.gitignore`**

```gitignore
/pulseguard
/pulseguard.exe
/data/
*.db
*.db-shm
*.db-wal
/.idea/
/.vscode/
/dist/
.env
config.yaml
```

- [ ] **Step 3: Write minimal `cmd/pulseguard/main.go`**

```go
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Println("pulseguard: not implemented yet")
	os.Exit(0)
}
```

- [ ] **Step 4: Write `Makefile`**

```makefile
.PHONY: build test fmt vet run
build:
	go build -o pulseguard ./cmd/pulseguard
test:
	go test ./...
fmt:
	go fmt ./...
vet:
	go vet ./...
run: build
	./pulseguard
```

- [ ] **Step 5: Verify build**

Run: `make build && ./pulseguard`
Expected output: `pulseguard: not implemented yet`

- [ ] **Step 6: Commit**

```bash
git add .
git commit -m "chore: bootstrap pulseguard skeleton (module, gitignore, stub main, Makefile)"
```

---

## Task 2: Config package (yaml + env)

**Files:**
- Create: `E:\a2026\PulseGuard\internal\config\config.go`
- Create: `E:\a2026\PulseGuard\internal\config\config_test.go`
- Create: `E:\a2026\PulseGuard\config.example.yaml`
- Modify: `go.mod` (adds gopkg.in/yaml.v3)

- [ ] **Step 1: Write `config.example.yaml`**

```yaml
server:
  listen_addr: ":8080"
  base_url: "http://localhost:8080"
  read_timeout: 10s
  write_timeout: 30s
  shutdown_timeout: 15s
database:
  path: "./data/pulseguard.db"
  busy_timeout: 5s
security:
  master_key_b64: ""        # 32-byte base64; required
  session_ttl: 336h         # 14d
  cookie_secure: true
  bcrypt_cost: 10
worker:
  count: 4
  poll_interval: 1s
  max_attempts: 6
  inflight_reclaim_after: 60s
  backoff_schedule: [1s, 5s, 15s, 60s, 5m, 15m]
telegram:
  api_base: "https://api.telegram.org"
  http_timeout: 10s
bootstrap:
  initial_admin_email: ""
  initial_admin_password: ""
logging:
  level: "info"
  format: "json"
cleanup:
  push_logs_keep_days: 30
  dedup_keys_sweep_interval: 1h
  sessions_sweep_interval: 1h
```

- [ ] **Step 2: Write failing test `internal/config/config_test.go`**

```go
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
	os.WriteFile(p, []byte("server:\n  listen_addr: \":8080\"\n"), 0644)
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
	os.WriteFile(p, []byte("{}"), 0644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.ReadTimeout != 10*time.Second {
		t.Fatalf("default ReadTimeout = %v", cfg.Server.ReadTimeout)
	}
	if cfg.Worker.MaxAttempts != 6 {
		t.Fatalf("default MaxAttempts = %d", cfg.Worker.MaxAttempts)
	}
}
```

- [ ] **Step 3: Run test (should fail)**

Run: `go test ./internal/config/...`
Expected: FAIL, `package config: no Go files`

- [ ] **Step 4: Write `internal/config/config.go`**

```go
package config

import (
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    Server    `yaml:"server"`
	Database  Database  `yaml:"database"`
	Security  Security  `yaml:"security"`
	Worker    Worker    `yaml:"worker"`
	Telegram  Telegram  `yaml:"telegram"`
	Bootstrap Bootstrap `yaml:"bootstrap"`
	Logging   Logging   `yaml:"logging"`
	Cleanup   Cleanup   `yaml:"cleanup"`
}

type Server struct {
	ListenAddr      string        `yaml:"listen_addr"      env:"LISTEN_ADDR"`
	BaseURL         string        `yaml:"base_url"         env:"BASE_URL"`
	ReadTimeout     time.Duration `yaml:"read_timeout"     env:"READ_TIMEOUT"`
	WriteTimeout    time.Duration `yaml:"write_timeout"    env:"WRITE_TIMEOUT"`
	ShutdownTimeout time.Duration `yaml:"shutdown_timeout" env:"SHUTDOWN_TIMEOUT"`
}

type Database struct {
	Path         string        `yaml:"path"          env:"PATH"`
	BusyTimeout  time.Duration `yaml:"busy_timeout"  env:"BUSY_TIMEOUT"`
}

type Security struct {
	MasterKeyB64 string        `yaml:"master_key_b64" env:"MASTER_KEY_B64"`
	SessionTTL   time.Duration `yaml:"session_ttl"    env:"SESSION_TTL"`
	CookieSecure bool          `yaml:"cookie_secure"  env:"COOKIE_SECURE"`
	BcryptCost   int           `yaml:"bcrypt_cost"    env:"BCRYPT_COST"`
}

type Worker struct {
	Count                int             `yaml:"count"                  env:"COUNT"`
	PollInterval         time.Duration   `yaml:"poll_interval"          env:"POLL_INTERVAL"`
	MaxAttempts          int             `yaml:"max_attempts"           env:"MAX_ATTEMPTS"`
	InflightReclaimAfter time.Duration   `yaml:"inflight_reclaim_after" env:"INFLIGHT_RECLAIM_AFTER"`
	BackoffSchedule      []time.Duration `yaml:"backoff_schedule"`
}

type Telegram struct {
	APIBase     string        `yaml:"api_base"     env:"API_BASE"`
	HTTPTimeout time.Duration `yaml:"http_timeout" env:"HTTP_TIMEOUT"`
}

type Bootstrap struct {
	InitialAdminEmail    string `yaml:"initial_admin_email"    env:"INITIAL_ADMIN_EMAIL"`
	InitialAdminPassword string `yaml:"initial_admin_password" env:"INITIAL_ADMIN_PASSWORD"`
}

type Logging struct {
	Level  string `yaml:"level"  env:"LEVEL"`
	Format string `yaml:"format" env:"FORMAT"`
}

type Cleanup struct {
	PushLogsKeepDays        int           `yaml:"push_logs_keep_days"         env:"PUSH_LOGS_KEEP_DAYS"`
	DedupKeysSweepInterval  time.Duration `yaml:"dedup_keys_sweep_interval"   env:"DEDUP_KEYS_SWEEP_INTERVAL"`
	SessionsSweepInterval   time.Duration `yaml:"sessions_sweep_interval"     env:"SESSIONS_SWEEP_INTERVAL"`
}

func Load(path string) (*Config, error) {
	cfg := defaults()
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(raw, cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	applyEnv(cfg, "PULSEGUARD")
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Server: Server{
			ListenAddr: ":8080", BaseURL: "http://localhost:8080",
			ReadTimeout: 10 * time.Second, WriteTimeout: 30 * time.Second,
			ShutdownTimeout: 15 * time.Second,
		},
		Database: Database{Path: "./data/pulseguard.db", BusyTimeout: 5 * time.Second},
		Security: Security{
			SessionTTL: 14 * 24 * time.Hour, CookieSecure: true, BcryptCost: 10,
		},
		Worker: Worker{
			Count: 4, PollInterval: time.Second, MaxAttempts: 6,
			InflightReclaimAfter: 60 * time.Second,
			BackoffSchedule: []time.Duration{
				time.Second, 5 * time.Second, 15 * time.Second,
				60 * time.Second, 5 * time.Minute, 15 * time.Minute,
			},
		},
		Telegram: Telegram{APIBase: "https://api.telegram.org", HTTPTimeout: 10 * time.Second},
		Logging:  Logging{Level: "info", Format: "json"},
		Cleanup: Cleanup{
			PushLogsKeepDays: 30,
			DedupKeysSweepInterval: time.Hour,
			SessionsSweepInterval: time.Hour,
		},
	}
}

// applyEnv walks struct fields tagged `env:"NAME"` and overrides with
// values of $<prefix>_<SECTION>_<NAME> if set. Sections come from yaml tag of parent.
func applyEnv(cfg *Config, prefix string) {
	v := reflect.ValueOf(cfg).Elem()
	t := v.Type()
	for i := 0; i < v.NumField(); i++ {
		section := strings.ToUpper(t.Field(i).Tag.Get("yaml"))
		sv := v.Field(i)
		st := sv.Type()
		for j := 0; j < sv.NumField(); j++ {
			tag := st.Field(j).Tag.Get("env")
			if tag == "" {
				continue
			}
			envName := prefix + "_" + section + "_" + tag
			raw, ok := os.LookupEnv(envName)
			if !ok {
				continue
			}
			if err := setField(sv.Field(j), raw); err != nil {
				panic(fmt.Errorf("env %s: %w", envName, err))
			}
		}
	}
}

func setField(f reflect.Value, raw string) error {
	switch f.Kind() {
	case reflect.String:
		f.SetString(raw)
	case reflect.Bool:
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return err
		}
		f.SetBool(b)
	case reflect.Int, reflect.Int64:
		if f.Type().String() == "time.Duration" {
			d, err := time.ParseDuration(raw)
			if err != nil {
				return err
			}
			f.SetInt(int64(d))
		} else {
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return err
			}
			f.SetInt(n)
		}
	default:
		return fmt.Errorf("unsupported kind %s", f.Kind())
	}
	return nil
}
```

- [ ] **Step 5: Add yaml dep and run tests**

```bash
go get gopkg.in/yaml.v3@v3.0.1
go test ./internal/config/... -v
```
Expected: all 3 tests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/config config.example.yaml go.mod go.sum
git commit -m "feat(config): yaml+env config loader with defaults"
```

---

## Task 3: Logging package (slog + redaction)

**Files:**
- Create: `E:\a2026\PulseGuard\internal\logging\logger.go`
- Create: `E:\a2026\PulseGuard\internal\logging\logger_test.go`

- [ ] **Step 1: Write failing test**

```go
package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestRedacts(t *testing.T) {
	var buf bytes.Buffer
	l := newForTest(&buf, "info", "json")
	l.Info("login attempt",
		"email", "x@y.com",
		"password", "hunter2",
		"bot_token", "12345:secret",
		"push_token", "abc-xyz",
		"master_key", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",
	)
	out := buf.String()
	for _, leak := range []string{"hunter2", "12345:secret", "abc-xyz", "AAAAAAA"} {
		if strings.Contains(out, leak) {
			t.Fatalf("leaked %q in log: %s", leak, out)
		}
	}
	if !strings.Contains(out, "REDACTED") {
		t.Fatalf("missing REDACTED marker: %s", out)
	}
	// still valid JSON
	var any map[string]any
	if err := json.Unmarshal(buf.Bytes(), &any); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
}

func TestLevel(t *testing.T) {
	var buf bytes.Buffer
	l := newForTest(&buf, "warn", "text")
	l.Info("noise")
	l.Warn("danger")
	out := buf.String()
	if strings.Contains(out, "noise") {
		t.Fatalf("info should be filtered at warn level: %s", out)
	}
	if !strings.Contains(out, "danger") {
		t.Fatalf("warn missing: %s", out)
	}
}
```

- [ ] **Step 2: Run test (should fail)**

Run: `go test ./internal/logging/... -v`
Expected: FAIL, undefined references.

- [ ] **Step 3: Write `internal/logging/logger.go`**

```go
package logging

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
)

var sensitiveKeys = map[string]struct{}{
	"password": {}, "bot_token": {}, "push_token": {}, "master_key": {},
	"master_key_b64": {}, "session": {}, "cookie": {}, "secret": {},
	"authorization": {},
}

type redactHandler struct{ inner slog.Handler }

func (r redactHandler) Enabled(ctx context.Context, lvl slog.Level) bool {
	return r.inner.Enabled(ctx, lvl)
}
func (r redactHandler) Handle(ctx context.Context, rec slog.Record) error {
	newRec := slog.NewRecord(rec.Time, rec.Level, rec.Message, rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		newRec.AddAttrs(redact(a))
		return true
	})
	return r.inner.Handle(ctx, newRec)
}
func (r redactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = redact(a)
	}
	return redactHandler{inner: r.inner.WithAttrs(out)}
}
func (r redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{inner: r.inner.WithGroup(name)}
}

func redact(a slog.Attr) slog.Attr {
	if _, ok := sensitiveKeys[strings.ToLower(a.Key)]; ok {
		return slog.String(a.Key, "REDACTED")
	}
	return a
}

// New configures and returns the application logger.
func New(level, format string) *slog.Logger { return newForTest(os.Stdout, level, format) }

func newForTest(w io.Writer, level, format string) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var inner slog.Handler
	if strings.ToLower(format) == "text" {
		inner = slog.NewTextHandler(w, opts)
	} else {
		inner = slog.NewJSONHandler(w, opts)
	}
	return slog.New(redactHandler{inner: inner})
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/logging/... -v`
Expected: both PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/logging
git commit -m "feat(logging): slog wrapper with key redaction"
```

---

## Task 4: Clock interface

**Files:**
- Create: `E:\a2026\PulseGuard\internal\domain\clock.go`

- [ ] **Step 1: Write file**

```go
package domain

import "time"

// Clock abstracts time.Now so workers and rate limiters can be tested deterministically.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// RealClock returns a Clock backed by time.Now.
func RealClock() Clock { return realClock{} }

// FakeClock is a deterministic clock for tests. NOT goroutine-safe.
type FakeClock struct{ T time.Time }

func (f *FakeClock) Now() time.Time     { return f.T }
func (f *FakeClock) Advance(d time.Duration) { f.T = f.T.Add(d) }
```

- [ ] **Step 2: Commit**

```bash
git add internal/domain/clock.go
git commit -m "feat(domain): clock interface for deterministic time"
```

---

## Task 5: Domain entities

**Files:**
- Create: `E:\a2026\PulseGuard\internal\domain\tenant.go`
- Create: `E:\a2026\PulseGuard\internal\domain\bot.go`
- Create: `E:\a2026\PulseGuard\internal\domain\template.go`
- Create: `E:\a2026\PulseGuard\internal\domain\channel.go`
- Create: `E:\a2026\PulseGuard\internal\domain\push.go`
- Create: `E:\a2026\PulseGuard\internal\domain\errors.go`

- [ ] **Step 1: Write `tenant.go`**

```go
package domain

import "time"

type Role string

const (
	RoleUser  Role = "user"
	RoleAdmin Role = "admin"
)

type TenantStatus string

const (
	TenantActive   TenantStatus = "active"
	TenantDisabled TenantStatus = "disabled"
)

type Tenant struct {
	ID           int64
	Email        string
	PasswordHash string
	DisplayName  string
	Role         Role
	Status       TenantStatus
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type InviteCode struct {
	Code      string
	CreatedBy int64
	UsedBy    *int64
	ExpiresAt *time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

type Session struct {
	ID        string
	TenantID  int64
	ExpiresAt time.Time
	CreatedAt time.Time
}
```

- [ ] **Step 2: Write `bot.go`, `template.go`, `channel.go`**

```go
// internal/domain/bot.go
package domain

import "time"

type Bot struct {
	ID          int64
	TenantID    int64
	Name        string
	BotToken    string    // plaintext (set after store-layer decryption)
	Description string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
```

```go
// internal/domain/template.go
package domain

import "time"

type ParseMode string

const (
	ParseMarkdownV2 ParseMode = "MarkdownV2"
	ParseHTML       ParseMode = "HTML"
	ParseNone       ParseMode = "None"
)

type Template struct {
	ID        int64
	TenantID  int64
	Name      string
	ParseMode ParseMode
	Body      string
	CreatedAt time.Time
	UpdatedAt time.Time
}
```

```go
// internal/domain/channel.go
package domain

import "time"

type Channel struct {
	ID            int64
	TenantID      int64
	Name          string
	PushToken     string
	BotID         int64
	TemplateID    int64
	ChatID        string
	RatePerMin    int
	DedupWindowS  int
	Enabled       bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
}
```

- [ ] **Step 3: Write `push.go`**

```go
package domain

import "time"

type OutboxStatus string

const (
	OutboxPending  OutboxStatus = "pending"
	OutboxInFlight OutboxStatus = "in_flight"
	OutboxSent     OutboxStatus = "sent"
	OutboxRetry    OutboxStatus = "retry"
	OutboxDead     OutboxStatus = "dead"
)

type PushOutbox struct {
	ID             int64
	ChannelID      int64
	TenantID       int64
	PayloadJSON    string
	DedupKey       *string
	Status         OutboxStatus
	Attempts       int
	NextAttemptAt  time.Time
	LastError      *string
	WorkerID       *string
	ClaimedAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type LogStatus string

const (
	LogSent   LogStatus = "sent"
	LogFailed LogStatus = "failed"
	LogDead   LogStatus = "dead"
)

type PushLog struct {
	ID             int64
	OutboxID       *int64
	ChannelID      int64
	TenantID       int64
	PayloadJSON    string
	RenderedText   string
	TGMessageID    *int64
	Status         LogStatus
	Error          *string
	Attempts       int
	CreatedAt      time.Time
}

type DeadLetter struct {
	ID            int64
	OutboxID      int64
	ChannelID     int64
	TenantID      int64
	PayloadJSON   string
	RenderedText  *string
	LastError     string
	Attempts      int
	CreatedAt     time.Time
}

type PushRequest struct {
	ChannelID int64
	TenantID  int64
	Payload   map[string]any
	DedupKey  string  // optional, from payload.dedup_key
}
```

- [ ] **Step 4: Write `errors.go`**

```go
package domain

import "errors"

var (
	ErrNotFound         = errors.New("not found")
	ErrUnauthorized     = errors.New("unauthorized")
	ErrForbidden        = errors.New("forbidden")
	ErrConflict         = errors.New("conflict")
	ErrValidation       = errors.New("validation")
	ErrRateLimited      = errors.New("rate_limited")
	ErrChannelDisabled  = errors.New("channel_disabled")
	ErrInviteInvalid    = errors.New("invite_invalid")
	ErrInternal         = errors.New("internal")
)
```

- [ ] **Step 5: Compile-check and commit**

```bash
go build ./internal/domain/...
git add internal/domain
git commit -m "feat(domain): entities (Tenant/Bot/Template/Channel/Outbox/Log/DL) and errors"
```

---

