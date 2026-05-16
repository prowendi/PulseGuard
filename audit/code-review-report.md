# PulseGuard 代码质量审核
日期：2026-05-17

## 摘要
- 总体评级：B+
- 阻塞性问题：2
- 应改进：8
- 加分项：9

整体架构清晰、分层合理，spec 覆盖度高。两个阻塞性问题均
涉及数据一致性与可观测性盲区，修复成本低但上线前必须解决。

---

## 阻塞性问题（必须修复才能上生产）

### C-B1 store/migrate.go 直接使用 time.Now() 绕过 Clock
- 文件：internal/store/migrate.go:124
- 现象：`runMigration` 在 INSERT schema_migrations 时调用
  `time.Now().UnixMilli()`，而非接受注入的 Clock。
- 影响：若未来需要幂等重跑迁移回放（如集成测试中验证迁
  移顺序），applied_at 不可控，测试将非确定性。更关键的
  是它打破了项目"所有时间戳经 Clock"的设计契约（spec
  §6），成为唯一的特例入口。
- 建议：让 `MigrateFS` 接受 `domain.Clock` 参数，或改用
  `store.nowMs(clock)` 的方式统一。变更面小且向后兼容。

### C-B2 Worker 内所有副作用操作静默丢弃错误
- 文件：internal/pipeline/worker.go:132,187,201,216-217,
  236,247-248
- 现象：`markSent/dlq/markRetryOrDead` 等方法中对
  `Outbox.MarkRetry`, `Logs.Insert`, `DLQ.Insert`,
  `Outbox.MarkDead` 的返回值全部 `_ = ...` 丢弃。
- 影响：
  1. 若 MarkSent 失败，outbox 行永远停在 in_flight，最终
     被 Cleanup 重新投递——用户收到重复消息。
  2. 若 DLQ.Insert 失败，死信丢失，无法回放审计。
  3. 若 Logs.Insert 失败，push 日志缺失，运维盲区。
  4. 没有任何日志记录这些内部失败——worker 缺乏 slog
     注入导致完全无可观测性。
- 建议：
  a. Worker 注入 `*slog.Logger`；每次 `_ = ...` 改为
     `if err := ...; err != nil { logger.Warn(...) }`。
  b. MarkSent/MarkDead 失败时做一次有限重试或 panic 触发
     recover 日志；至少保证可观测。

---

## 应改进

### C-I1 Worker 无 slog 注入——生产不可观测
- 文件：internal/pipeline/worker.go:68-72
- 现象：注释写道"Logging is the caller's responsibility"，
  但实际上 runtime/run.go 从未向 Worker 传递 logger，致使
  claim/sent/retry/dead 关键事件在生产环境中完全静默。
- 建议：在 `WorkerDeps` 中增加 `Logger *slog.Logger` 字段
  并在 tick 各分支输出结构化日志（含 worker_id, channel_id,
  outbox_id, attempts）。

### C-I2 RateLimitRepo.Allow 未使用 BEGIN IMMEDIATE
- 文件：internal/store/ratelimit_repo.go:41
- 现象：`r.db.BeginTx(ctx, nil)` 以 deferred 模式开启事务，
  SELECT 时不持有写锁。在 4 个 worker 同时竞争同一 channel
  时，两个 goroutine 可能都读到 tokens>=1 后各 UPDATE -1，
  导致实际消耗 2 但只该放行 1。
- 影响：限流偶发失效，突发量超过配置上限。
- 建议：使用 `_txlock=immediate` DSN 参数，或执行
  `BEGIN IMMEDIATE` 手动 SQL（modernc/sqlite 支持）。

### C-I3 auth.Service.Register 非原子——Lock 与 Consume 分事务
- 文件：internal/auth/service.go:49-108
- 现象：注释承认"SQLite has no cross-repo transaction"。
  Lock 在 invite_repo 内开独立事务（读后 rollback 释放锁），
  Consume 又开新事务。二者之间有窗口：
  1. Thread A Lock 成功 -> Thread B Lock 同一 code 成功
  2. Thread A Insert tenant, Consume -> ok
  3. Thread B Insert tenant (email 冲突 → ErrConflict → fail)
     但 invite code 的 Lock 已释放，B 不会消耗 code。
  虽然最终结果不会腐蚀数据（email unique 兜底），但 B 收到
  的错误语义不清晰：应该是 ErrInviteInvalid 而非 Conflict。
- 建议：将 Lock + tenant Insert + Consume 包裹到同一个
  `db.BeginTx(ctx, &sql.TxOptions{})` 事务中（InviteRepo
  提供 WithTx 方法已预留此能力但未使用）。

### C-I4 CSRF token 在首次访问时未颁发——GET 请求无 cookie
- 文件：internal/web/csrf.go + middleware/csrf.go
- 现象：CSRF cookie 仅在 login/register 成功后通过
  `IssueCSRF` 写入。若用户先进入 /ui/dashboard（已有
  session 但无 psg_csrf cookie），首次 POST 操作将因
  "missing csrf cookie" 被 403。
- 建议：在 RequireAuth 中间件中检测 psg_csrf cookie 是否
  存在，若缺失则自动 Issue 一次。或在 layout.html 渲染时
  注入。

### C-I5 push_api.go 未关闭 r.Body
- 文件：internal/web/push_api.go:44
- 现象：`json.NewDecoder(r.Body)` 读取后 Body 未显式
  Close。虽然 net/http 在 handler 返回后自动 drain，但若
  body 超大（恶意客户端发 100MB），解码器仅读到第一个 JSON
  值后返回，剩余字节仍挂在连接上直到 handler 返回——此期间
  占用内存。
- 建议：添加 `http.MaxBytesReader` 限制 body 大小
  （spec 未定义上限，建议 1MB），或在 decodeJSON 通用方法
  中统一限制。

### C-I6 Cleanup ticker 不走 Clock——无法确定性测试
- 文件：internal/pipeline/cleanup.go:62-68
- 现象：`time.NewTicker` 使用真实时钟。Cleanup.Run 在单测
  中只能设置极短 interval 再 sleep——违反"禁止 sleep"的
  测试纪律（spec §6）。
- 建议：将 ticker 抽象为可注入的 `func() <-chan time.Time`
  或使用 domain.Clock + 手动 channel 通知。

### C-I7 DedupRepo 事务中 SELECT 后 INSERT 存在 race window
- 文件：internal/store/dedup_repo.go:39-65
- 现象：`BeginTx(ctx, nil)` 以 deferred 模式打开——与
  C-I2 类似，两个并发 goroutine 可同时 SELECT "no rows"
  然后各自 INSERT，第二个 INSERT 因 PK (channel_id, fp)
  冲突而失败——但返回的 error 不是 `alreadySeen=true`
  而是一个裸 SQL 错误。
- 建议：改用 `INSERT ... ON CONFLICT DO UPDATE` (UPSERT)
  原子操作，或确保事务以 IMMEDIATE 模式获取写锁。

### C-I8 HTTP 日志中间件缺少 request_id 关联
- 文件：internal/web/middleware/logger.go:42-50
- 现象：Logger 输出 method/path/status/dur/tenant_id，但
  未输出 request_id（chi.RequestID 已在上层设置到 header）。
  运维在高并发下无法将 error response 的 request_id 与
  access log 行关联。
- 建议：从 r.Context() 或 response header 取
  X-Request-Id 并追加到 slog attrs。

---

## 加分项 / 已做得好的地方

1. **domain 零外部依赖**——repo.go 仅 import context/time，
   实体无 SQL/HTTP 泄漏，接口设计精确。
2. **单向包依赖严格执行**——`go vet ./...` 通过、无循环
   import。
3. **ClaimNext SQL 原子性**——UPDATE...RETURNING + 子查询
   LIMIT 1 + ORDER BY id ASC 防饥饿，设计正确。
4. **Bot token AES-GCM 加密存储**——Cipher 实现规范：
   nonce 前缀、Overhead 校验、crypto/rand。
5. **CSRF 双重 cookie+header 校验**——constant-time
   compare、form fallback 兼容非 HTMX 表单。
6. **日志脱敏完整**——9 个关键字段在 slog handler 层统一
   redact，无法被上层绕过。
7. **编译时接口检查**——所有 store repo 末尾
   `var _ domain.XxxRepo = (*XxxRepo)(nil)` 防漂移。
8. **graceful shutdown 完整**——srv.Shutdown + wg.Wait +
   WAL checkpoint + db.Close 顺序正确。
9. **Error envelope 统一**——writeError 带 request_id，
   writeRepoError 做 domain error 到 HTTP status 的集中
   映射，CRUD handler 极简。

---

## 优先级 1 重构清单（8 条）

1. Worker 注入 slog.Logger，消除所有 `_ = err` 盲区
   (C-B2 + C-I1)。
2. RateLimitRepo / DedupRepo 改用 BEGIN IMMEDIATE 或
   UPSERT 保证并发安全 (C-I2 + C-I7)。
3. store/migrate.go 时间戳走 Clock (C-B1)。
4. auth.Register 单事务化 (C-I3)。
5. CSRF cookie 在 RequireAuth 中自动补发 (C-I4)。
6. push endpoint 添加 MaxBytesReader(1MB) (C-I5)。
7. Cleanup ticker 注入可测试时间源 (C-I6)。
8. Logger 中间件输出 request_id (C-I8)。

---

## 命名 / API 一致性微调建议

| 位置 | 现状 | 建议 |
|------|------|------|
| web/bot_api.go:21 | `BotTokenLast4` | 改 `TokenHint`（与 spec 无明确对齐，但更通用） |
| web/responses.go:35 | `writeError` request_id 从 `r.Header` 取 | chi RequestID 设置在 **response** header，应只从 `w.Header().Get("X-Request-Id")` 取，当前代码已 fallback 但优先级反了 |
| pipeline/worker.go:46 | `New` | 建议 `NewWorker` 避免与 package-level 歧义 |
| config.Duration 类型 | 散落在所有 config 子结构 | 考虑 `type Duration = config.Duration` 在 domain 暴露别名减少 store/tg 对 config 包的 import |
| web/deadletter_api.go | Replay 返回 202 | spec §4.4 未指定 status；202 合理，但建议在 spec 补齐 |
