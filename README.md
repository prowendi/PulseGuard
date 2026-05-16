# PulseGuard

PulseGuard 是一个面向 SaaS 多租户的 **告警推送基建平台**：用户在平台上配置自己的 Telegram Bot、消息模板与推送通道（Channel），监控/CI/业务系统通过通用 HTTP Webhook (`POST /api/v1/push/{token}`) 把告警 payload 推入平台，后台 Outbox Worker 异步消费、渲染、限流、去重、调用 Telegram Bot API 投递。

整套系统打成 **单可执行文件 + 一份 SQLite**，无 Redis / 无 MQ / 无外部依赖，可以塞进一台 1C1G 的 VPS。

设计目标：让一个对监控告警有需求的小团队，在 5 分钟内拿到一个有"重试 / 死信 / 限流 / 历史 / 多租户 / 邀请码注册"的生产级告警通道。

---

## 特性

- 单 binary 部署：Go 1.25 + 纯 Go SQLite (`modernc.org/sqlite`)，无 cgo，跨平台
- 多租户：邀请码注册，admin 可签发 / 撤销邀请码
- 通用 Webhook：`POST /api/v1/push/{push_token}` 收任意 JSON
- 异步 Outbox 管线：HTTP 立即返回 202，后台 N 个 worker 消费投递
- 重试 / 死信：指数退避 `[1s, 5s, 15s, 60s, 5m, 15m]`，最终失败入 DLQ，UI 一键重投
- Token Bucket 限流：channel 级 `rate_per_min`，超额自动延后
- 去重窗口：`dedup_key` 或 payload 指纹，窗口期内重复消息自动丢弃
- 模板系统：Go `text/template` 全语法，支持 MarkdownV2 / HTML / 纯文本，内置 escape 函数
- HTMX UI：单 binary 内嵌前端资源，零额外构建步骤
- Bot Token 加密入库：AES-GCM + 启动时校验 32B master_key
- TG API 错误分类：Transient / PermanentClient / PermanentServer，429 自动用 `retry_after`
- Graceful Shutdown：SIGTERM → 关 listener → flush WAL → 退出，崩溃恢复回收 in_flight

---

## 快速开始（Docker）

```bash
docker run -d --name pulseguard \
  -p 8080:8080 \
  -v $(pwd)/data:/var/lib/pulseguard \
  -e PULSEGUARD_SECURITY_MASTER_KEY_B64=$(openssl rand -base64 32) \
  -e PULSEGUARD_BOOTSTRAP_INITIAL_ADMIN_EMAIL=admin@example.com \
  -e PULSEGUARD_BOOTSTRAP_INITIAL_ADMIN_PASSWORD=ChangeMeNow \
  ghcr.io/wendi/pulseguard:latest
```

打开 `http://localhost:8080/ui/login`，用上面的 admin 邮箱/密码登录，立即开始配置 bot/template/channel。

> 生产部署务必：(1) 用 `openssl rand -base64 32` 生成长期密钥并妥善保管；(2) 在反向代理（Caddy/Nginx）上做 TLS 终止；(3) 把 `PULSEGUARD_SECURITY_COOKIE_SECURE=true`。

---

## 本地开发

前置：Go 1.25+。

```bash
git clone https://github.com/wendi/pulseguard.git
cd pulseguard
make build
./pulseguard -config config.example.yaml
```

`config.example.yaml` 自带可直接运行的 dev 默认值（cookie_secure=false、bootstrap admin 已填）。首次启动检测到空库时会按 `bootstrap.initial_admin_*` 创建第一个 admin 租户。

常用 make targets：

| target | 用途 |
|--------|------|
| `make build` | 本平台编译，产物 `./pulseguard` |
| `make test` | 全量单测 |
| `make cover` | 带覆盖率 |
| `make lint` | `go vet ./... && go build ./...` |
| `make docker` | `docker build -t pulseguard:dev .` |
| `make release` | 跨平台编译到 `dist/`（linux/amd64, linux/arm64, windows/amd64, darwin/amd64, darwin/arm64） |
| `make smoke` | 真实 Telegram 烟雾测试，需要环境变量 `PULSEGUARD_SMOKE_BOT_TOKEN` 和 `PULSEGUARD_SMOKE_CHAT_ID` |

---

## 核心概念

```
Tenant ──┬── Bot         （bot_token 加密入库，可被多个 channel 复用）
         ├── Template    （Go text/template，parse_mode = MarkdownV2 / HTML / None）
         └── Channel     （1 push_token → 1 bot + 1 chat_id + 1 template，静态绑定）
```

| 对象 | 说明 |
|------|------|
| **Tenant** | 一个邮箱 = 一个租户。所有 Bot/Template/Channel 都属于某个 tenant，强隔离 |
| **Bot** | Telegram Bot 凭证，`bot_token` AES-GCM 加密落库 |
| **Template** | 消息模板，支持条件 / 循环 / escape 函数（`escMD`, `escHTML`） |
| **Channel** | 推送通道，唯一 `push_token` 是对外的 webhook 入口 |
| **push_token** | 32 字符 URL-safe 随机串，channel 级隔离，UI 可一键轮换 |
| **Outbox** | 推送写入 `push_outbox` 表，状态机 `pending → in_flight → sent / retry / dead` |
| **DLQ** | 终态失败的消息进 `dead_letters`，UI 可重投回 outbox |

---

## API 用例

完整流程：登录 → 建 bot → 建 template → 建 channel → 推消息。

```bash
BASE=http://localhost:8080

# 1) 登录，拿 session cookie
curl -c cookie.txt -X POST $BASE/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"admin@example.com","password":"ChangeMeNow!2026"}'

# 2) 创建一个 bot（bot_token 入库即加密）
curl -b cookie.txt -X POST $BASE/api/v1/bots \
  -H 'Content-Type: application/json' \
  -d '{"name":"ops-bot","bot_token":"123456:ABC-DEF...","description":"prod alert bot"}'

# 3) 创建一个模板
curl -b cookie.txt -X POST $BASE/api/v1/templates \
  -H 'Content-Type: application/json' \
  -d '{
    "name":"alert-md",
    "parse_mode":"MarkdownV2",
    "body":"{{ if eq .level \"critical\" }}\\ud83d\\udea8{{ else }}\\u26a0\\ufe0f{{ end }} *{{ .title | escMD }}*\\n\\nHost: `{{ .host | escMD }}`\\nValue: *{{ .value | escMD }}*"
  }'

# 4) 创建 channel（绑定 bot_id + template_id + chat_id），返回 push_token
curl -b cookie.txt -X POST $BASE/api/v1/channels \
  -H 'Content-Type: application/json' \
  -d '{
    "name":"prod-cpu",
    "bot_id":1,
    "template_id":1,
    "chat_id":"-1001234567890",
    "rate_per_min":60,
    "dedup_window_s":300
  }'
# → 201 { "id":1, "push_token":"K8x...abc", ... }

# 5) 推一条告警（无需登录，凭 push_token 鉴权）
curl -X POST $BASE/api/v1/push/K8x...abc \
  -H 'Content-Type: application/json' \
  -d '{
    "title":"CPU High",
    "host":"db01.prod",
    "level":"critical",
    "value":"95%",
    "dedup_key":"cpu_db01"
  }'
# → 202 { "push_id":12345, "status":"queued" }
```

---

## 配置

完整字段表（值取自 `config.example.yaml`，env 覆盖规则 `PULSEGUARD_<SECTION>_<FIELD>`）：

### `server`
| 字段 | 默认 | 说明 |
|------|------|------|
| `listen_addr` | `:8080` | HTTP 监听地址 |
| `base_url` | `http://localhost:8080` | 对外 URL，用于 UI 展示 push_token 完整链接 |
| `read_timeout` | `10s` | 请求读超时 |
| `write_timeout` | `30s` | 请求写超时 |
| `shutdown_timeout` | `15s` | graceful shutdown 等 worker 收尾的上限 |

### `database`
| 字段 | 默认 | 说明 |
|------|------|------|
| `path` | `./data/pulseguard.db` | SQLite 文件路径，目录自动创建 |
| `busy_timeout` | `5s` | `PRAGMA busy_timeout`，写锁争用退让窗口 |

### `security`
| 字段 | 默认 | 说明 |
|------|------|------|
| `master_key_b64` | **REQUIRED** | 32B base64 主密钥，用于 AES-GCM 加密 bot_token |
| `session_ttl` | `336h` (14d) | session cookie TTL |
| `cookie_secure` | `true` | 生产 https=true，本地 http=false |
| `bcrypt_cost` | `10` | bcrypt 哈希轮数 |

### `worker`
| 字段 | 默认 | 说明 |
|------|------|------|
| `count` | `4` | outbox worker goroutine 数 |
| `poll_interval` | `1s` | 空闲轮询休眠 |
| `max_attempts` | `6` | 单条最大尝试次数，超过入 DLQ |
| `inflight_reclaim_after` | `60s` | 僵尸 in_flight 行回收阈值 |
| `backoff_schedule` | `[1s, 5s, 15s, 60s, 5m, 15m]` | 第 N 次失败的退避间隔 |

### `telegram`
| 字段 | 默认 | 说明 |
|------|------|------|
| `api_base` | `https://api.telegram.org` | TG Bot API 基础 URL |
| `http_timeout` | `10s` | TG API 整体超时 |

### `bootstrap`
| 字段 | 默认 | 说明 |
|------|------|------|
| `initial_admin_email` | **REQUIRED** | 空库时自动创建的 admin 邮箱 |
| `initial_admin_password` | **REQUIRED** | 初始 admin 密码，首次登录后建议立即修改 |

### `logging`
| 字段 | 默认 | 说明 |
|------|------|------|
| `level` | `info` | debug / info / warn / error |
| `format` | `json` | json 或 text |

### `cleanup`
| 字段 | 默认 | 说明 |
|------|------|------|
| `push_logs_keep_days` | `30` | push_logs 保留天数 |
| `dedup_keys_sweep_interval` | `1h` | 过期 dedup_keys 清理周期 |
| `sessions_sweep_interval` | `1h` | 过期 sessions 清理周期 |

---

## 运维

### 日志

`logging.format=json` 时输出标准 slog JSON，可直接被 vector / promtail / fluentbit 抓取。敏感字段（`bot_token`, `push_token`, `password`, `master_key`）自动脱敏。

### 备份

PulseGuard 不内置备份，运维方按文件系统/卷快照的方式备份：

```bash
# 单文件冷备（停服）
systemctl stop pulseguard
cp /var/lib/pulseguard/pulseguard.db /backup/$(date +%F).db
systemctl start pulseguard

# 在线热备（推荐，不停服）
sqlite3 /var/lib/pulseguard/pulseguard.db ".backup /backup/$(date +%F).db"
```

### Master Key 轮转

`master_key_b64` 用于加密 bot_token。轮转步骤（停服窗口）：

1. 用旧 key 启动 PulseGuard，导出全部 bot：`GET /api/v1/bots` 拿到明文 token
2. 生成新 key：`openssl rand -base64 32`
3. 停服，备份 db
4. 用 **新 key + 旧 token 重新 POST** 每个 bot（或写一个迁移脚本：旧 key 解密 → 新 key 加密）
5. 用新 key 重启，验证发送

> 当前 MVP 没有内置「双 key 滚动解密」，因此轮转需要短暂停服。后续可以加 `master_key_b64_previous` 配合双 key 解密路径。

### 升级

单 binary 替换即可，schema migration 在启动时自动 idempotent 跑：

```bash
systemctl stop pulseguard
cp pulseguard-linux-amd64 /usr/local/bin/pulseguard
systemctl start pulseguard
journalctl -fu pulseguard
```

---

## 限制 / 非目标（V1 MVP）

为防止范围蔓延，以下功能显式排除在 MVP 之外：

- Webhook HMAC 签名（push_token 已经做了 channel 级隔离）
- Channel Group / fan-out（一对多推送）
- 静默窗口 / 值班排班 / on-call
- Prometheus Alertmanager 协议兼容
- SMTP 接收（邮件转 TG）
- 邮箱验证 / 密码重置邮件
- OAuth (GitHub / Google) 登录
- ACK / 升级机制
- Telegram client 集群化
- `/metrics` Prometheus 暴露（端点占位，未实现）
- 自动数据库备份（按文件备份）
- 计费 / 配额硬上限
- 国际化 / i18n

---

## License

MIT (TODO: 正式发布前补 LICENSE 文件)
