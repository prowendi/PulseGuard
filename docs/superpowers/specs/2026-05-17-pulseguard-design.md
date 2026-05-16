# PulseGuard — 设计文档 (Spec)

> **状态**：Approved (2026-05-17)
> **作者**：与用户协作 (brainstorming session)
> **下一步**：交付到 `writing-plans` 生成实施计划

## 摘要

PulseGuard 是一个面向 SaaS 多租户的告警推送基建平台。用户在平台上配置自己的 Telegram Bot Key、消息模板与推送通道（Channel），通过通用 HTTP Webhook (`POST /api/v1/push/{token}`) 把告警 payload 推入平台；后台 Outbox Worker 异步消费、渲染、限流、去重、调用 Telegram Bot API 投递；失败按指数退避重试，最终失败入死信队列。整套系统打成单可执行文件 + SQLite，无外部依赖。

---

## §1 架构总览

- **项目名**：PulseGuard
- **技术栈**：Go (≥1.22) + SQLite (modernc.org/sqlite，纯 Go，免 cgo) + HTMX + Tailwind (CDN 或 embed CSS) + Go html/template
- **交付物**：单 binary（embed 静态资源与模板）+ 一份 `config.yaml`
- **租户模型**：SaaS 多租户，邀请码注册
- **接入面**：通用 HTTP Webhook `POST /api/v1/push/{token}`
- **后台架构**：HTTP Server + Outbox Worker Pool + Cleanup Worker 共进程

### 顶层组件拓扑

```
┌──────────────────────────────────────────────────────────────────┐
│                    pulseguard (single binary)                    │
│                                                                  │
│   ┌─────────────────┐         ┌──────────────────────┐           │
│   │  HTTP Server    │         │  Outbox Worker Pool  │           │
│   │  (chi router)   │         │  (N goroutines)      │           │
│   │                 │         │                      │           │
│   │  /api/v1/push   │         │  ┌─────────────────┐ │           │
│   │  /api/v1/...    │         │  │ tick (1s poll)  │ │           │
│   │  /ui/*  (HTMX)  │         │  └────────┬────────┘ │           │
│   │  /healthz       │         │           ▼          │           │
│   └────────┬────────┘         │  ┌─────────────────┐ │           │
│            │                  │  │ render template │ │           │
│            │                  │  │ rate-limit check│ │           │
│            │                  │  │ call TG API     │ │           │
│            │                  │  │ retry / DLQ     │ │           │
│            │                  │  └────────┬────────┘ │           │
│            ▼                  │           │          │           │
│   ┌─────────────────────────────┐         │          │           │
│   │   SQLite (WAL mode, 1 file) │◄────────┘          │           │
│   │   - tenants/invite_codes    │                    │           │
│   │   - bots/templates/channels │                    │           │
│   │   - push_outbox / push_logs │                    │           │
│   │   - dedup_keys / rate_buckets                    │           │
│   │   - dead_letters / sessions │                    │           │
│   └─────────────────────────────┘                    │           │
│                                                      │           │
└──────────────────────────────────────────────────────┴───────────┘
                            │
                            ▼ HTTPS
                    api.telegram.org
```

### 关键架构决策（已锁定）

| 决策 | 选型 | 理由 |
|------|------|------|
| 部署形态 | 单 binary + SQLite 文件 | 零依赖，运维成本最低 |
| 推送管线 | Outbox + Worker 异步 | MVP 治理（重试/死信/限流/历史）的承载基础 |
| 数据库 | SQLite WAL 模式 | 单机够用、并发读优秀；纯 Go 驱动免 cgo |
| 前端 | HTMX + 后端模板 | 单 binary、无独立前端构建 |
| 认证 | 邀请码注册 + session cookie | 免 SMTP/OAuth |
| 路由库 | go-chi/chi v5 | 标准库风格，中间件清晰 |
| 接入面 | 仅通用 HTTP Webhook | 单一抓手，Prometheus/SMTP 排进 V2 |
| 通道模型 | 静态绑定（1 token → 1 channel → 1 bot + 1 chat_id + 1 template） | YAGNI |
| Bot 存储 | 池化（bots 表独立，channels 外键引用） | 同 bot 复用多 chat |
| 模板能力 | Go text/template 全语法 | 灵活，能处理列表/条件 |
| Fan-out | 不支持（1:1） | V2 通过 ChannelGroup 引入 |

### 进程内并发模型

- **HTTP Server**：标准 `net/http.Server` + chi，多核并行
- **Outbox Worker**：N 个 goroutine（默认 4，可配），通过 SQL `UPDATE ... RETURNING` 抢占任务避免重复消费
- **Cleanup Worker**：1 个 goroutine，每小时跑：清理过期 session、过期 dedup_keys、归档老 push_logs（保留 N 天）
- **Graceful Shutdown**：SIGTERM → 关 HTTP listener → 等正在跑的 worker 完成（最多 15s）→ flush WAL → 退出

---

## §2 组件拆分与代码组织

### 目录结构

```
pulseguard/
├── cmd/pulseguard/main.go        # 进程入口：load config → wire → start
│
├── internal/
│   ├── domain/                   # 纯实体 + 业务错误，zero external dep
│   │   ├── tenant.go             # Tenant, InviteCode, Session
│   │   ├── bot.go                # Bot
│   │   ├── template.go           # Template (parse_mode, body)
│   │   ├── channel.go            # Channel (token, bot_id, chat_id, ...)
│   │   ├── push.go               # PushRequest, PushOutbox, PushLog, DeadLetter
│   │   ├── repo.go               # 各仓储接口
│   │   └── errors.go             # ErrNotFound, ErrUnauthorized, ErrRateLimited, ...
│   │
│   ├── store/                    # SQLite 持久层，所有 SQL 都在这
│   │   ├── db.go                 # *sql.DB + WAL/migrations 初始化
│   │   ├── tenant_repo.go
│   │   ├── bot_repo.go
│   │   ├── template_repo.go
│   │   ├── channel_repo.go
│   │   ├── outbox_repo.go        # push_outbox 抢占 SQL
│   │   ├── log_repo.go
│   │   ├── dedup_repo.go
│   │   ├── ratelimit_repo.go
│   │   ├── deadletter_repo.go
│   │   ├── session_repo.go
│   │   ├── invite_repo.go
│   │   └── migrations/
│   │       └── 0001_init.sql     # embed FS
│   │
│   ├── auth/
│   │   ├── service.go            # Register / Login / Logout / SessionFromCookie
│   │   ├── invite.go             # GenerateInvite / ConsumeInvite
│   │   └── hash.go               # bcrypt 封装
│   │
│   ├── tg/
│   │   ├── client.go             # SendMessage(...) 实现 domain.Sender
│   │   └── errors.go             # Transient / PermanentClient / PermanentServer
│   │
│   ├── render/
│   │   └── render.go             # Render(tpl, payload) (string, error) + 转义函数
│   │
│   ├── pipeline/
│   │   ├── ingest.go             # Ingest(req PushRequest) → 写 outbox
│   │   ├── worker.go             # Worker.Run(ctx) 主循环
│   │   ├── ratelimit.go          # token-bucket on SQLite
│   │   ├── dedup.go              # 指纹判定
│   │   ├── retry.go              # 指数退避计划
│   │   └── cleanup.go            # Cleanup worker
│   │
│   ├── web/
│   │   ├── server.go             # chi router 装配
│   │   ├── middleware/
│   │   │   ├── auth.go           # session 校验 → 注入 tenant 到 ctx
│   │   │   ├── logger.go
│   │   │   ├── recover.go
│   │   │   └── ratelimit.go      # 全局 IP 级 chi/httprate
│   │   ├── api/
│   │   │   ├── push.go           # POST /api/v1/push/{token}
│   │   │   ├── bot.go            # /api/v1/bots CRUD
│   │   │   ├── channel.go
│   │   │   ├── template.go
│   │   │   ├── log.go            # GET /api/v1/logs?...
│   │   │   ├── dlq.go            # GET/POST /api/v1/deadletters (含重投)
│   │   │   ├── invite.go         # /api/v1/invites (admin)
│   │   │   └── auth.go           # /api/v1/auth/register|login|logout
│   │   └── ui/
│   │       └── handler.go        # HTMX 渲染端点
│   │
│   ├── config/
│   │   └── config.go             # yaml + env 合并
│   │
│   └── logging/
│       └── logger.go             # slog 配置 + 脱敏
│
├── web/                          # 前端资源（go:embed）
│   ├── templates/
│   │   ├── layout.html
│   │   ├── login.html
│   │   ├── register.html
│   │   ├── dashboard.html
│   │   ├── bots.html
│   │   ├── channels.html
│   │   ├── templates.html
│   │   ├── logs.html
│   │   ├── deadletters.html
│   │   └── partials/
│   │       ├── nav.html
│   │       ├── flash.html
│   │       └── *_row.html        # HTMX 列表行片段
│   └── static/
│       ├── htmx.min.js
│       ├── app.css               # 或用 Tailwind CDN
│       └── app.js
│
├── docs/superpowers/
│   ├── specs/2026-05-17-pulseguard-design.md
│   └── plans/
├── go.mod
└── go.sum
```

### Package 依赖方向（单向）

```
                     ┌────────┐
                     │ domain │
                     └────┬───┘
                          │  (所有 package 引用 domain 类型/接口)
        ┌─────────────────┼───────────────────┐
        ▼                 ▼                   ▼
    ┌────────┐        ┌────────┐         ┌────────┐
    │ store  │        │   tg   │         │ render │
    └────┬───┘        └────┬───┘         └────┬───┘
         │                 │                  │
         └──────┬──────────┴──────────────────┘
                ▼
          ┌──────────┐         ┌──────┐
          │ pipeline │ ◀────── │ auth │
          └────┬─────┘         └──┬───┘
               │                  │
               └────────┬─────────┘
                        ▼
                    ┌───────┐
                    │  web  │ ◀── cmd/pulseguard/main.go (wire)
                    └───────┘
```

- 箭头方向唯一，内环包永远不知道外环包存在
- `domain/repo.go` 用接口声明各仓储能力，`store` 实现这些接口；测试用 in-memory fake 替代
- `main.go` 是唯一 wire 点（~150 行）：装配 config → db → repos → services → server

### 关键接口（domain/repo.go）

```go
package domain

type TenantRepo interface {
    Insert(ctx context.Context, t *Tenant) error
    GetByEmail(ctx context.Context, email string) (*Tenant, error)
    GetByID(ctx context.Context, id int64) (*Tenant, error)
    CountActive(ctx context.Context) (int, error)
}

type InviteRepo interface {
    Insert(ctx context.Context, code *InviteCode) error
    Lock(ctx context.Context, code string) (*InviteCode, error) // SELECT ... FOR UPDATE 语义（事务内）
    Consume(ctx context.Context, code string, tenantID int64) error
    ListByCreator(ctx context.Context, adminID int64) ([]*InviteCode, error)
}

type BotRepo interface {
    Insert(ctx context.Context, b *Bot) error
    Update(ctx context.Context, b *Bot) error
    Delete(ctx context.Context, tenantID, id int64) error
    GetByID(ctx context.Context, tenantID, id int64) (*Bot, error)
    ListByTenant(ctx context.Context, tenantID int64) ([]*Bot, error)
}

type TemplateRepo interface {
    Insert(ctx context.Context, t *Template) error
    Update(ctx context.Context, t *Template) error
    Delete(ctx context.Context, tenantID, id int64) error
    GetByID(ctx context.Context, tenantID, id int64) (*Template, error)
    ListByTenant(ctx context.Context, tenantID int64) ([]*Template, error)
}

type ChannelRepo interface {
    Insert(ctx context.Context, c *Channel) error
    Update(ctx context.Context, c *Channel) error
    Delete(ctx context.Context, tenantID, id int64) error
    GetByID(ctx context.Context, tenantID, id int64) (*Channel, error)
    GetByPushToken(ctx context.Context, pushToken string) (*Channel, error)
    ListByTenant(ctx context.Context, tenantID int64) ([]*Channel, error)
}

type OutboxRepo interface {
    Insert(ctx context.Context, item *PushOutbox) (int64, error)
    ClaimNext(ctx context.Context, workerID string, now time.Time) (*PushOutbox, error)
    MarkSent(ctx context.Context, id int64, now time.Time) error
    MarkRetry(ctx context.Context, id int64, nextAt time.Time, reason string) error
    MarkDead(ctx context.Context, id int64, reason string) error
    ReclaimInFlight(ctx context.Context, olderThan time.Time) (int64, error)
}

type LogRepo interface {
    Insert(ctx context.Context, log *PushLog) error
    ListByTenant(ctx context.Context, tenantID int64, page, perPage int) ([]*PushLog, int, error)
    ListByChannel(ctx context.Context, tenantID, channelID int64, page, perPage int) ([]*PushLog, int, error)
    PurgeOlderThan(ctx context.Context, t time.Time) (int64, error)
}

type DedupRepo interface {
    SeenOrInsert(ctx context.Context, channelID int64, fp string, now time.Time, windowSec int) (alreadySeen bool, err error)
    PurgeExpired(ctx context.Context, now time.Time) (int64, error)
}

type RateLimiter interface {
    Allow(ctx context.Context, channelID int64, ratePerMin int) (bool, error)
}

type DeadLetterRepo interface {
    Insert(ctx context.Context, dl *DeadLetter) error
    ListByTenant(ctx context.Context, tenantID int64, page, perPage int) ([]*DeadLetter, int, error)
    Replay(ctx context.Context, tenantID, id int64) (newOutboxID int64, err error) // 复制 payload 到 outbox
}

type SessionRepo interface {
    Insert(ctx context.Context, s *Session) error
    GetByID(ctx context.Context, id string) (*Session, error)
    Delete(ctx context.Context, id string) error
    PurgeExpired(ctx context.Context, now time.Time) (int64, error)
}

type Sender interface { // tg 包实现
    Send(ctx context.Context, botToken, chatID, parseMode, text string) (msgID int64, err error)
}

type Clock interface {
    Now() time.Time
}
```

### 测试边界

| 层 | 工具 | 覆盖 |
|---|------|------|
| domain | 标准 testing | 实体不变量、错误类型 |
| store | testing + SQLite `file::memory:?cache=shared` | 全部 repo CRUD、迁移、并发 ClaimNext |
| pipeline | testing + fake repos | retry/backoff、去重、限流数学 |
| tg | `httptest.Server` | 错误分类、429 retry_after |
| web | `httptest.NewRecorder` + 真 SQLite + fake Sender | push → outbox → log 端到端 |

---

## §3 数据模型 (SQLite Schema)

> 时间统一 `INTEGER unix_ts_ms`；外键全开 `PRAGMA foreign_keys=ON`；写采用 `BEGIN IMMEDIATE` 避免读写冲突。

```sql
-- ===== migrations/0001_init.sql =====

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
PRAGMA busy_timeout = 5000;

-- ─── 租户 / 邀请码 / 会话 ─────────────────────────────
CREATE TABLE tenants (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  email           TEXT    NOT NULL UNIQUE,
  password_hash   TEXT    NOT NULL,
  display_name    TEXT    NOT NULL DEFAULT '',
  role            TEXT    NOT NULL DEFAULT 'user' CHECK (role IN ('user','admin')),
  status          TEXT    NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE TABLE invite_codes (
  code            TEXT    PRIMARY KEY,
  created_by      INTEGER NOT NULL REFERENCES tenants(id),
  used_by         INTEGER REFERENCES tenants(id),
  expires_at      INTEGER,
  used_at         INTEGER,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_invite_unused ON invite_codes(used_at) WHERE used_at IS NULL;

CREATE TABLE sessions (
  id              TEXT    PRIMARY KEY,
  tenant_id       INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  expires_at      INTEGER NOT NULL,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_sessions_tenant ON sessions(tenant_id);
CREATE INDEX idx_sessions_expires ON sessions(expires_at);

-- ─── 资源：Bot / Template / Channel ──────────────────
CREATE TABLE bots (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id       INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name            TEXT    NOT NULL,
  bot_token_enc   BLOB    NOT NULL,
  description     TEXT    NOT NULL DEFAULT '',
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE (tenant_id, name)
);

CREATE TABLE templates (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id       INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name            TEXT    NOT NULL,
  parse_mode      TEXT    NOT NULL DEFAULT 'MarkdownV2'
                  CHECK (parse_mode IN ('MarkdownV2','HTML','None')),
  body            TEXT    NOT NULL,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE (tenant_id, name)
);

CREATE TABLE channels (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  tenant_id       INTEGER NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name            TEXT    NOT NULL,
  push_token      TEXT    NOT NULL UNIQUE,
  bot_id          INTEGER NOT NULL REFERENCES bots(id) ON DELETE RESTRICT,
  template_id     INTEGER NOT NULL REFERENCES templates(id) ON DELETE RESTRICT,
  chat_id         TEXT    NOT NULL,
  rate_per_min    INTEGER NOT NULL DEFAULT 60,
  dedup_window_s  INTEGER NOT NULL DEFAULT 0,
  enabled         INTEGER NOT NULL DEFAULT 1,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL,
  UNIQUE (tenant_id, name)
);
CREATE INDEX idx_channels_tenant ON channels(tenant_id);

-- ─── 推送管线：Outbox / Logs / DLQ ───────────────────
CREATE TABLE push_outbox (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id      INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  tenant_id       INTEGER NOT NULL,
  payload_json    TEXT    NOT NULL,
  dedup_key       TEXT,
  status          TEXT    NOT NULL DEFAULT 'pending'
                  CHECK (status IN ('pending','in_flight','sent','retry','dead')),
  attempts        INTEGER NOT NULL DEFAULT 0,
  next_attempt_at INTEGER NOT NULL,
  last_error      TEXT,
  worker_id       TEXT,
  claimed_at      INTEGER,
  created_at      INTEGER NOT NULL,
  updated_at      INTEGER NOT NULL
);
CREATE INDEX idx_outbox_claim ON push_outbox(status, next_attempt_at)
  WHERE status IN ('pending','retry');

CREATE TABLE push_logs (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  outbox_id       INTEGER REFERENCES push_outbox(id) ON DELETE SET NULL,
  channel_id      INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  tenant_id       INTEGER NOT NULL,
  payload_json    TEXT    NOT NULL,
  rendered_text   TEXT    NOT NULL,
  tg_message_id   INTEGER,
  status          TEXT    NOT NULL CHECK (status IN ('sent','failed','dead')),
  error           TEXT,
  attempts        INTEGER NOT NULL,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_logs_channel_time ON push_logs(channel_id, created_at DESC);
CREATE INDEX idx_logs_tenant_time  ON push_logs(tenant_id, created_at DESC);

CREATE TABLE dead_letters (
  id              INTEGER PRIMARY KEY AUTOINCREMENT,
  outbox_id       INTEGER NOT NULL,
  channel_id      INTEGER NOT NULL,
  tenant_id       INTEGER NOT NULL,
  payload_json    TEXT    NOT NULL,
  rendered_text   TEXT,
  last_error      TEXT    NOT NULL,
  attempts        INTEGER NOT NULL,
  created_at      INTEGER NOT NULL
);
CREATE INDEX idx_dlq_tenant ON dead_letters(tenant_id, created_at DESC);

-- ─── 治理：限流 / 去重 ──────────────────────────────
CREATE TABLE rate_buckets (
  channel_id      INTEGER PRIMARY KEY REFERENCES channels(id) ON DELETE CASCADE,
  tokens          REAL    NOT NULL,
  updated_at      INTEGER NOT NULL
);

CREATE TABLE dedup_keys (
  channel_id      INTEGER NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
  fingerprint     TEXT    NOT NULL,
  first_seen_at   INTEGER NOT NULL,
  last_seen_at    INTEGER NOT NULL,
  hit_count       INTEGER NOT NULL DEFAULT 1,
  expires_at      INTEGER NOT NULL,
  PRIMARY KEY (channel_id, fingerprint)
);
CREATE INDEX idx_dedup_expires ON dedup_keys(expires_at);
```

**Bot Token 加密**：`bots.bot_token_enc` 以 AES-GCM 加密入库，密钥来自 `config.security.master_key_b64`（32B base64，启动校验长度）。Nonce 与密文一起存到 BLOB（前 12 字节是 nonce）。

---

## §4 数据流（核心场景）

### 4.1 Push 请求全链路

```
[Client]
   │  POST /api/v1/push/{push_token}
   │  Content-Type: application/json
   │  Body: { "title":"CPU high", "host":"db01", "value":"95%",
   │          "dedup_key":"cpu_db01" }   ← 可选
   ▼
[web/api/push.go]
   1. 用 push_token 查 channel → ErrNotFound → 404
   2. 校验 channel.enabled → 否 → 410 Gone
   3. 解析 body 为 JSON → 失败 → 400
   4. (若 channel.dedup_window_s > 0) 计算 fingerprint:
        fp = body.dedup_key OR SHA256(规范化 body)
      调 dedup.SeenOrInsert → 命中则 200 {dropped:true, reason:"dedup"}
   5. pipeline.Ingest(channel, payload):
        - INSERT push_outbox (status='pending', next_attempt_at=now)
        - 返回 push_id
   6. 立即返回 202 Accepted { "push_id": 12345, "status": "queued" }

[Worker (每个 goroutine)]
   每 1s tick:  (以下为流程伪代码；实际调用以 §2 接口为准——LogRepo.Insert 接 *PushLog 构造体，
                  DeadLetterRepo.Insert 接 *DeadLetter 构造体)
   for {
     item := OutboxRepo.ClaimNext(workerID, now)
     if item == nil { sleep poll_interval; continue }

     ch  := ChannelRepo.Get(item.channel_id)
     bot := BotRepo.Get(ch.bot_id)            (解密 bot_token)
     tpl := TemplateRepo.Get(ch.template_id)

     if !RateLimiter.Allow(ch.id, ch.rate_per_min) {
       OutboxRepo.MarkRetry(item.id, now+1s, "rate-limited")
       continue
     }

     text, err := render.Render(tpl, item.payload)
     if err != nil {  // 模板错误是永久性的
       LogRepo.WriteFailed(item, "", err)
       DeadLetterRepo.Insert(item, err)
       OutboxRepo.MarkDead(item.id, err.Error())
       continue
     }

     msgID, err := tg.Send(ctx, bot.token, ch.chat_id, tpl.parse_mode, text)
     switch classify(err) {
       case nil:
         LogRepo.WriteSent(item, text, msgID)
         OutboxRepo.MarkSent(item.id, now)
       case Transient:                    // 5xx / network / 429
         if item.attempts+1 >= max_attempts {
           LogRepo.WriteFailed(item, text, err)
           DeadLetterRepo.Insert(item, err)
           OutboxRepo.MarkDead(item.id, err.Error())
         } else {
           OutboxRepo.MarkRetry(item.id, now+backoff(item.attempts+1), err.Error())
         }
       case Permanent:                    // 4xx 非 429 / token 失效
         LogRepo.WriteFailed(item, text, err)
         DeadLetterRepo.Insert(item, err)
         OutboxRepo.MarkDead(item.id, err.Error())
     }
   }
```

**ClaimNext 的 SQL**（防多 worker 抢同一行；SQLite 3.35+ 支持 `RETURNING`）：

```sql
UPDATE push_outbox
   SET status = 'in_flight',
       worker_id = ?,
       claimed_at = ?,
       updated_at = ?,
       attempts = attempts + 1
 WHERE id = (
   SELECT id FROM push_outbox
    WHERE status IN ('pending','retry')
      AND next_attempt_at <= ?
    ORDER BY next_attempt_at ASC
    LIMIT 1
 )
 RETURNING id, channel_id, tenant_id, payload_json, dedup_key,
           status, attempts, next_attempt_at, last_error,
           worker_id, claimed_at, created_at, updated_at;
```

**Backoff 计划**：`[1s, 5s, 15s, 60s, 5m, 15m]`，`max_attempts = 6`。

### 4.2 注册 / 登录

```
[register] POST /api/v1/auth/register  { email, password, invite_code }
   ↓
  auth.Register():
    BEGIN
      1. invite_repo.Lock(code)       → 不存在/已用/过期 → 4xx
      2. tenant_repo.Insert(email, bcrypt(password), role='user')
      3. invite_repo.Consume(code, tenantID)
    COMMIT
      4. 颁发 session cookie
   ↓
  302 → /ui/dashboard

[login]  POST /api/v1/auth/login  { email, password }
   ↓
  auth.Login():
    1. tenant_repo.GetByEmail
    2. bcrypt.Compare
    3. session_repo.Insert(id=randomB32, tenant_id, ttl=14d)
    4. Set-Cookie: psg_session=<id>; HttpOnly; Secure; SameSite=Lax
   ↓
  302 → /ui/dashboard

[logout] POST /api/v1/auth/logout
   ↓ 删 session 行 + 清 cookie
```

### 4.3 配置 CRUD（以 Channel 创建为例）

```
UI 表单 → POST /ui/channels (HTMX)
       └─ API JSON → POST /api/v1/channels

共用 service:
  ValidateInput → 自动生成 push_token (URL-safe 32 chars)
  → channel_repo.Insert(tenant_id 取自 ctx)
  → 返回 created channel

UI 处理器把结果渲染为列表行片段，HTMX swap 到 #channel-list
API 处理器返回 201 JSON
```

### 4.4 死信回放

```
UI 列表点"重投"按钮 → POST /ui/deadletters/{id}/replay
  service.ReplayDeadLetter(tenant_id, dlq_id):
    BEGIN
      dl := deadletter_repo.GetByID
      校验 dl.tenant_id == ctx.tenant_id
      outbox_id := outbox_repo.Insert(channel_id=dl.channel_id, payload=dl.payload, attempts=0)
      （DLQ 行保留，标 replayed_at 字段；本 MVP 简化为直接保留不动）
    COMMIT
  返回新 outbox_id
```

---

## §5 错误处理与可靠性

### 5.1 TG API 错误分类（tg/errors.go）

| 类别 | 触发 | 处置 |
|------|------|------|
| `Transient` | 网络超时、5xx、429（含 `parameters.retry_after`） | 退避重试；429 用 retry_after 覆盖默认 backoff |
| `PermanentClient` | 400 `Bad Request: chat not found`、`bot was blocked by user` | 直接 DLQ |
| `PermanentServer` | 401 `Unauthorized`（token 失效）、403 非 blocked | DLQ |
| `Unknown` | 解析失败 | 当 Transient 处理（保守） |

`Sender` 返回的 error 用类型实现分类：

```go
type Class int
const (
    Transient Class = iota
    PermanentClient
    PermanentServer
)
type TGError struct { Class Class; Code int; Description string; RetryAfter time.Duration }
func (e *TGError) Error() string { ... }
```

### 5.2 进程崩溃恢复

- **抢占语义**：worker `ClaimNext` 将 status 改为 `in_flight` + `worker_id` + `claimed_at`
- **启动回收**：进程启动时 / 每分钟一次 `UPDATE push_outbox SET status='retry', worker_id=NULL WHERE status='in_flight' AND claimed_at < ?`（参数=`now - 60s`，可配）
- **WAL checkpoint**：默认 SQLite auto-checkpoint；进程退出前 `PRAGMA wal_checkpoint(TRUNCATE)`

### 5.3 HTTP 错误响应格式

```json
{ "error": { "code": "RATE_LIMITED", "message": "...", "request_id": "..." } }
```

错误码全集：`UNAUTHORIZED / FORBIDDEN / NOT_FOUND / VALIDATION / RATE_LIMITED / CHANNEL_DISABLED / CONFLICT / INTERNAL`。

### 5.4 限流（Token Bucket on SQLite）

每 channel 一行 `rate_buckets`。`Allow(channelID, ratePerMin)`：

```
BEGIN IMMEDIATE
  SELECT tokens, updated_at FROM rate_buckets WHERE channel_id = ?
  if not exists: INSERT (channel_id, tokens=ratePerMin, updated_at=now)
  else:
    elapsed_ms = now - updated_at
    tokens = min(ratePerMin, tokens + elapsed_ms/60000 * ratePerMin)
  if tokens >= 1:
    tokens -= 1
    UPDATE rate_buckets SET tokens=?, updated_at=? WHERE channel_id=?
    return true
  else:
    UPDATE rate_buckets SET updated_at=? WHERE channel_id=?  -- 不补 token，但记 ts
    return false
COMMIT
```

容量 = `ratePerMin`（允许 1 分钟突发）。`ratePerMin=0` 视为不限。

### 5.5 死信处理

- DLQ 表不自动清理，长期保留供审计
- UI 提供"重投"按钮（4.4）
- 后续可加"DLQ 满 N 条告警"提醒，本 MVP 不做

---

## §6 测试策略

| 层级 | 工具 | 覆盖 | 目标覆盖率 |
|------|------|------|---------|
| 单测 - domain | testing | 实体不变量、错误类型 | 100% |
| 单测 - render | testing + table tests | 模板渲染、parse_mode 转义、坏模板 fail-fast | 90%+ |
| 单测 - pipeline | testing + fake repos | retry/backoff、去重、限流数学 | 85%+ |
| 集成测 - store | testing + 真 SQLite memory | 全部 repo CRUD、迁移、并发 ClaimNext | 关键路径 100% |
| 集成测 - tg | `httptest.Server` | 错误分类、429 retry_after | 关键路径 100% |
| 端到端 - web | `httptest` + 真 SQLite + fake Sender | push → outbox → log 主路径与 4 个失败路径 | 主路径 100% |
| 手动验收 | 真 TG Bot + curl | smoke：注册→配置→push→收到 | — |

**测试纪律（写入 plan）**：

- worker / cleanup 测试一律走 `Clock` 接口注入 `fakeClock`，禁用 `time.Sleep`
- 并发测试用 `sync.WaitGroup` + 超时 `context`
- 等待异步生效的断言用 `eventually(t, 5s, func() bool { ... })` 助手，不用 sleep
- table tests 优先；任何 `if err != nil { t.Fatal }` 都要有上下文（fmt.Errorf 包裹）

---

## §7 配置与部署

### 7.1 `config.yaml`

```yaml
server:
  listen_addr: ":8080"
  base_url: "https://pulseguard.example.com"
  read_timeout: 10s
  write_timeout: 30s
  shutdown_timeout: 15s

database:
  path: "./data/pulseguard.db"
  busy_timeout: 5s

security:
  master_key_b64: "${PULSEGUARD_MASTER_KEY}"  # 32B base64，必填
  session_ttl: 336h                            # 14d
  cookie_secure: true                          # dev 改 false
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
  initial_admin_email: "admin@example.com"
  initial_admin_password: "${PULSEGUARD_INITIAL_PASSWORD}"

logging:
  level: "info"
  format: "json"

cleanup:
  push_logs_keep_days: 30
  dedup_keys_sweep_interval: 1h
  sessions_sweep_interval: 1h
```

**env 覆盖规则**：所有 yaml 字段可被 `PULSEGUARD_<UPPER_SNAKE>` 覆盖，路径用 `_` 分隔（如 `PULSEGUARD_SERVER_LISTEN_ADDR`）。

### 7.2 启动顺序

```
main():
  1. cfg := config.Load("./config.yaml")    # yaml + env merge
  2. logger := logging.New(cfg.Logging)
  3. db := store.Open(cfg.Database)
       db.Migrate(embeddedFS)
       db.ReclaimInFlightOutbox(now - cfg.Worker.InflightReclaimAfter)
  4. repos := store.NewRepos(db)
  5. tgClient := tg.New(cfg.Telegram)
  6. clock := realClock{}
  7. authSvc := auth.New(repos, cfg.Security, clock)
  8. pipe := pipeline.New(repos, tgClient, cfg.Worker, clock)
  9. mux := web.NewServer(cfg, repos, authSvc, pipe, embeddedWebFS)
 10. bootstrap.EnsureInitialAdmin(repos, cfg.Bootstrap)
 11. errgroup with rootCtx:
       go httpServer.Serve(mux)
       go pipe.Workers.Run(ctx)
       go pipe.Cleanup.Run(ctx)
       wait for SIGTERM/SIGINT
 12. graceful shutdown:
       httpServer.Shutdown(ctx, cfg.Server.ShutdownTimeout)
       cancel rootCtx → workers exit
       db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
       db.Close()
```

### 7.3 交付物

- `pulseguard` 单可执行（Linux/amd64, Linux/arm64, Windows/amd64, macOS）
- `config.example.yaml`
- `Dockerfile`（FROM scratch，COPY binary + config）
- `systemd/pulseguard.service`（示例）

---

## §8 安全考量

| 维度 | 措施 |
|------|------|
| 凭证存储 | `bot_token` AES-GCM 加密（master_key），`password_hash` bcrypt(cost=10) |
| Session | HttpOnly + Secure + SameSite=Lax，TTL 14d，登出立删 |
| CSRF | 所有 state-mutating UI 请求要求 `X-CSRF-Token` header（HTMX 全局配置发送），token 与 session 绑定 |
| Push token | 32 字符 URL-safe (`crypto/rand` 24B → base64url)，channel 级隔离，UI 一键轮换 |
| 限流 | Channel 级 token bucket（§5.4）；全局 IP 级 100 req/s（chi httprate）防爬 |
| SQL 注入 | 全 SQL 用 `?` 占位符（database/sql）；不拼接 |
| TG 转义 | render 包提供 `EscapeMarkdownV2` / `EscapeHTML` 函数，模板里 `{{ .title \| escMD }}` |
| 日志脱敏 | logger middleware 屏蔽 `bot_token` / `push_token` / `password` / `master_key` 字段 |
| 多租户越权 | 所有 repo 方法签名带 `tenantID`；wire 层从 session 取 tenantID 注入 ctx；handler 不接受用户传入的 tenant_id |
| TLS | 不内置，由前置 reverse proxy (Caddy/Nginx) 终止 |

---

## §9 非目标（NOT in MVP）

为防止范围蔓延，显式排除：

- ❌ Webhook 签名 (HMAC)
- ❌ Channel Group / fan-out 一对多推送
- ❌ 静默窗口 / 值班排班 / on-call
- ❌ Prometheus Alertmanager 协议兼容
- ❌ SMTP 接收（邮件转 TG）
- ❌ 邮箱验证、密码重置邮件
- ❌ OAuth (GitHub/Google) 登录
- ❌ ACK / 升级机制
- ❌ Telegram client 集群化
- ❌ Prometheus metrics 暴露（`/metrics` 端点先留位但不实现）
- ❌ 自动数据库备份（运维方按文件备份）
- ❌ 计费 / 配额硬上限
- ❌ 国际化 / i18n（仅中英文混排，按 dev 默认）

---

## 附录 A — 核心 API 端点清单

```
公开（无需 session）:
  POST   /api/v1/auth/register     { email, password, invite_code }
  POST   /api/v1/auth/login        { email, password }
  POST   /api/v1/push/{token}      { ...任意 JSON ... }
  GET    /healthz                  → 200 ok
  GET    /ui/login, /ui/register   HTMX 页面

需要 session:
  POST   /api/v1/auth/logout
  GET    /api/v1/me

  CRUD /api/v1/bots
  CRUD /api/v1/templates
  CRUD /api/v1/channels
  GET  /api/v1/logs?channel_id=&page=&per_page=
  GET  /api/v1/deadletters?page=&per_page=
  POST /api/v1/deadletters/{id}/replay
  POST /api/v1/channels/{id}/rotate-token

  UI 同上 (路径前缀 /ui/...，返回 HTMX 片段)

管理员（role='admin'）:
  CRUD /api/v1/invites
  GET  /api/v1/admin/tenants
```

## 附录 B — 推送请求示例

```bash
curl -X POST https://pulseguard.example.com/api/v1/push/abc123def456 \
  -H "Content-Type: application/json" \
  -d '{
    "title": "CPU High",
    "host": "db01.prod",
    "level": "critical",
    "value": "95%",
    "url": "https://grafana.example.com/d/abc",
    "dedup_key": "cpu_db01"
  }'
# → 202 { "push_id": 12345, "status": "queued" }
```

模板示例（MarkdownV2）：

```
{{ if eq .level "critical" }}🚨{{ else }}⚠️{{ end }} *{{ .title | escMD }}*

Host: `{{ .host | escMD }}`
Value: *{{ .value | escMD }}*
{{ if .url }}[查看详情]({{ .url }}){{ end }}
```
