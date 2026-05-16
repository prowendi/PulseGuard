# PulseGuard 测试覆盖与质量审计

日期：2026-05-17

---

## 摘要

- **全仓覆盖率（有测试的包加权估算）**：约 76%
- **race detector**：FAIL — `pipeline` 包存在 1 处数据竞争
- **总测试数**：158 个（含子测试）
- **高优先级缺口**：6 项

---

## 覆盖率明细（vs spec §6）

| 包 | 实测覆盖率 | spec 目标 | 是否达标 | 备注 |
|---|---|---|---|---|
| domain | 0.0% | 100% | 不达标 | 无测试文件；但实体逻辑极薄，主要是类型定义 |
| render | 97.2% | 90% | 达标 | 超出目标 |
| pipeline | 79.1% | 85% | 不达标 | 差 ~6pp；见高优先级缺口 T-H1~H3 |
| store | 77.0% | 关键路径 100% | 部分达标 | CRUD 覆盖良好，边界路径有缺口 |
| tg | 82.2% | 关键路径 100% | 部分达标 | 网络超时路径未覆盖 |
| web | 58.0% | 主路径 100% | 不达标 | middleware 子包 0%；UI 端点覆盖浅 |
| auth | 71.1% | — | — | Register/Login 覆盖好，密码强度验证缺失 |
| bootstrap | 82.4% | — | — | 较好 |
| config | 58.1% | — | — | env 覆盖路径少 |
| logging | 69.2% | — | — | 脱敏测试覆盖有限 |
| runtime | 78.4% | — | — | 单 E2E 测试，细节路径未覆盖 |
| web/middleware | 0.0% | — | 不达标 | CSRFCheck、RequireAuth、RequireAdmin 无单测 |
| cmd/pulseguard | 0.0% | — | — | 入口文件，可接受 |

---

## 高优先级测试缺口（必须补）

### T-H1 worker：Transient + RetryAfter 解析路径——fakeOutbox 不验证 NextAttemptAt

- **场景**：`TestWorkerTransientRetryAfter` 已存在，但验证的是
  `fakeOutbox.MarkRetry` 写入的内存字段；没有对应的
  `store/OutboxRepo` 集成级测试，确认 `next_attempt_at` 真正落入
  SQLite 并被 `ClaimNext` 在正确时间读出。
- **为什么重要**：RetryAfter 是 Telegram 429 限速的核心恢复路径；
  如果 SQL 层写入或读取有 off-by-one（ms vs s 单位混淆），429
  消息将无法在指定时间内被重试，直接影响可靠性。
- **建议测试设计**：在 `store/outbox_repo_test.go` 增加一个集成测
  试：`MarkRetry` 写入 `now+7s`，在 `now+6s` 时 `ClaimNext` 应返回
  nil，在 `now+7s` 时应成功 claim。该测试已有框架（见
  `TestOutboxRepo_MarkRetry_DelaysNextClaim`），只需补充精确等于边
  界点 `nextAt` 的断言。

---

### T-H2 worker：ReclaimInFlight 启动时回收——缺少进程级集成验证

- **场景**：`TestCleanupReclaimInflight` 测试的是 `Cleanup` 结构体
  调用 `fakeOutboxForCleanup.ReclaimInFlight`（fake 实现），不是真
  实 SQL。`store/TestOutboxRepo_ReclaimInFlight` 验证了 SQL，但没有
  端到端验证"进程启动时会执行一次回收"这个行为
  （`runtime/run.go` 中的 `ReclaimInFlightOutbox` 调用链）。
- **为什么重要**：崩溃恢复的核心 SLA。若启动时回收路径被重构，
  现有测试无法发现回归。
- **建议测试设计**：在 `runtime/run_test.go` 增加用例：手动向 DB
  插入一条 `status='in_flight'、claimed_at=2小时前` 的 outbox 行，
  然后调用 `startRuntime`；等 runtime 就绪后，查询该行 status，应
  已变为 `retry`。

---

### T-H3 dedup：expires_at == now 的边界点行为

- **场景**：`store/dedup_repo.go:72` 的判断是
  `existingExpires <= nowMs`（`<=` 表示等于时也视为已过期），但
  `store/TestDedupRepo_AfterExpiryMisses` 只测了 `now+11s`（窗口
  10s），没有测 `now+10s`（恰好等于 `expires_at`）的情况。
- **为什么重要**：边界点语义决定 dedup 的精确窗口长度。`<=` vs
  `<` 差 1ms，影响业务合同。若将来修改为 `<`，现有测试不会失败。
- **建议测试设计**：在 `TestDedupRepo_AfterExpiryMisses` 后追加子
  用例：首次插入 `window=10`，再以 `now+exactly 10s`
  调用 `SeenOrInsert`，预期返回 `seen=false`（过期）；以
  `now+9.999s` 调用，预期返回 `seen=true`（仍活跃）。

---

### T-H4 web/middleware：CSRFCheck、RequireAuth、RequireAdmin 无单测

- **场景**：`internal/web/middleware/` 包报告覆盖率 0%。三个核心中
  间件（`csrf.go`、`auth.go`）没有任何直接测试。
  `TestCSRFTokensRoundTrip`（`web/server_test.go`）测的是
  `web.IssueCSRF` / `web.VerifyCSRF`，不是 `middleware.CSRFCheck()`。
- **为什么重要**：CSRF cookie 缺失、token 不匹配、cookie 存在但
  header 为空这三条路径是安全关键路径；session cookie 缺失、session
  过期、角色不足也是关键路径。现有 web 集成测试隐式覆盖了部分，但
  中间件本身的单元行为没有明确断言。
- **建议测试设计**：用 `httptest.NewRequest` + `httptest.NewRecorder`
  为 `middleware.CSRFCheck()` 写表驱动测试，覆盖：
  (1) cookie 缺失 → 403；
  (2) header 缺失 → 403；
  (3) cookie 存在、header 不匹配 → 403；
  (4) 两者匹配 → 200；
  (5) GET 方法绕过检查 → 200。
  `RequireAuth` 同理：cookie 缺失、session 不存在、session 过期、
  成功。`RequireAdmin` 加：role=user → 403 / redirect。

---

### T-H5 多租户越权：Template 和 Channel 的跨租户 GET/UPDATE/DELETE

- **场景**：`bot_test.go` 有 `TestBotsAPIRejectsCrossTenant`
  （仅 GET）；`channel_test.go` 有跨租户 FK 验证。但 Template
  资源无任何跨租户测试；Channel 和 Bot 的跨租户 PUT/DELETE 也没有
  测试（只测了 GET）。
- **为什么重要**：越权删改是 SaaS 安全的核心红线。repo 层所有方法
  都带 `tenantID`，但 handler 层如果错误地取参数（如从 URL path 取
  ID 后忘记加 tenantID 过滤），repo 仍能找到数据。
- **建议测试设计**：为 Bot、Template、Channel 各补充：
  (1) 跨租户 GET → 404；
  (2) 跨租户 PUT → 404；
  (3) 跨租户 DELETE → 404。
  利用 `web/testhelpers_test.go` 的 `testHarness`，两套账号分别
  登录即可。

---

### T-H6 graceful shutdown：worker 正在处理时 ctx cancel

- **场景**：`TestWorkerRunRespectsContext` 验证空队列下取消退出。
  `TestRunEndToEnd` 验证正常发送后关闭。但没有测试"worker 正在
  调用 `Sender.Send` 时收到 ctx cancel"的情况——是否等当前消息处
  理完、是否泄漏 goroutine、是否 double-mark 同一条 outbox 行。
- **为什么重要**：SIGTERM 期间 outbox 行状态不一致（in_flight
  永久卡住）是生产中最常见的数据问题。`ReclaimInFlight` 是兜底，
  但应该先验证正常 shutdown 路径是否干净。
- **建议测试设计**：在 `pipeline/worker_test.go` 中：
  (1) 将 `fakeSender.Send` 改为阻塞（通过 channel 控制），
  (2) 入队一条 outbox，
  (3) 启动 `w.Run`，
  (4) 等 Send 开始阻塞后 cancel ctx，
  (5) 解除 Send 阻塞，
  (6) 等 Run 返回，
  (7) 断言 outbox 行最终处于 sent/retry/dead 之一，不是 in_flight。

---

## 中优先级缺口

### T-M1 ratelimit：跨秒边界与突发行为

- `TestRateLimitRepo_PartialRefill` 覆盖了 10s 补充 10 token 的场景，
  但没有测试：
  (1) `elapsed < 1ms`（refill 为 0）时桶不变；
  (2) 突发：`ratePerMin=5`，1 次调用后连续 4 次应全部通过（初始
  容量 = ratePerMin，第一次调用写入 `tokens=4`，后 4 次消耗完）；
  (3) `ratePerMin` 变化（channel 更新）后首次调用桶容量应 clamp。
- 引用：`store/ratelimit_repo.go:53`（初始 tokens 计算）。

---

### T-M2 render：ParseMode=None 时不应输出转义字符

- 现有测试全用 `MarkdownV2` 或 `HTML` 验证转义，但 `ParseMode=None`
  时模板渲染应原样输出（不转义）。`TestRenderSimpleVariable` 用的
  是 domain.Template 默认零值（ParseMode 为空），与 None 不等价。
- 引用：`render/render.go`（FuncMap 注册处）。

---

### T-M3 tg：HTTP 超时路径

- `TestSendNetworkErrorTransient` 关闭了 server 后立即 Send，触发
  `connection refused`，不是真正的 context deadline exceeded。没有
  测试 `http.Client.Timeout` 超时后返回 `Transient` 的路径。
- 引用：`tg/client.go`。

---

### T-M4 auth：`SessionFromID` 返回的 tenant 状态校验

- `auth/service_test.go` 有 `TestSessionFromIDExpired`，但没有测试
  session 有效、但 tenant.Status=disabled 时是否被拒绝。
  `auth/service.go` 中是否有此检查未确认。
- 引用：`auth/service.go:SessionFromID`。

---

### T-M5 store：并发 RateLimitRepo.Allow

- `TestRateLimitRepo_ExhaustsCapacityThenRefills` 是顺序调用。
  `rate_buckets` 用 `BEGIN IMMEDIATE` 串行化，但没有类似
  `TestOutboxRepo_ClaimNext_Concurrent` 的并发测试验证不会出现
  `double-allow`（两个 goroutine 都消耗最后一个 token）。
- 引用：`store/ratelimit_repo.go:40`（BeginTx）。

---

### T-M6 config：env 覆盖路径

- `TestEnvOverride` 仅测了 1 个字段（`PULSEGUARD_SERVER_LISTEN_ADDR`）。
  嵌套字段（如 `PULSEGUARD_SECURITY_MASTER_KEY_B64`）和 Duration
  类型字段的 env 覆盖未测。
- 引用：`config/config.go`。

---

## flaky 风险

以下 `time.Sleep` / `time.After` 出现在测试代码中，逐一评估：

| 位置 | 用法 | 风险评级 | 说明 |
|---|---|---|---|
| `pipeline/cleanup_test.go:184` | `time.Sleep(30ms)` | **中** | `TestCleanupRunRespectsContext` 睡 30ms 期待 cleanup 跑 "几个 tick"（10ms 间隔）。在 CI 高负载时可能睡眠不足导致 0 次 tick，但该测试只断言能正常退出，不断言 tick 次数，因此实际失败概率低。 |
| `pipeline/cleanup_test.go:191` | `time.After(2s)` | 低 | 作为超时 guard 使用，合理。 |
| `pipeline/worker_test.go:549` | `time.Sleep(20ms)` | **高** | `TestWorkerRunRespectsContext` 睡 20ms 让 worker "spin a bit"，然后 cancel。在极慢机器上可能 cancel 在 worker 启动前发生，测试正确退出但没有真正验证 "运行中取消"。结合 race detector 失败（见下），该测试需要重写。 |
| `pipeline/worker_test.go:556` | `time.After(2s)` | 低 | 超时 guard，合理。 |
| `pipeline/worker_test.go:580` | `time.Sleep(10ms)` | **高** | `TestWorkerRunProcessesItem` 在 goroutine 中运行 `w.Run` 同时主 goroutine 轮询 `f.outbox.get(item.ID).Status`。**这是已确认的数据竞争来源**：`fakeOutbox.ClaimNext` 在 goroutine 中修改 `it.Status`（持锁），但主 goroutine 通过 `get()` 直接读取 `items[id]` 的引用副本，没有加锁保护读操作。 |
| `runtime/run_test.go:135` | `time.Sleep(20ms)` 在 `eventually` 内 | 低 | `eventually` 有显式超时，sleep 只是轮询间隔，合理。 |
| `runtime/run_test.go:214,221,245` | `time.After(3s/5s)` | 低 | 超时 guard，合理。 |

**结论**：`TestWorkerRunProcessesItem` 存在已确认数据竞争（race
detector 输出见下文），必须修复。

---

## race detector 输出（摘录）

```
WARNING: DATA RACE
Write at 0x00c0001f4240 by goroutine 73:
  (*fakeOutbox).ClaimNext()
      worker_test.go:55
  (*Worker).tick()
      worker.go:92
  (*Worker).Run()
      worker.go:66
  TestWorkerRunProcessesItem.func2()
      worker_test.go:570

Previous read at 0x00c0001f4240 by goroutine 72:
  TestWorkerRunProcessesItem()
      worker_test.go:575
```

**根因**：`fakeOutbox.ClaimNext` 在 goroutine 内持 `mu` 锁修改
`it.Status`（`worker_test.go:55`），但 `fakeOutbox.get` 返回
`f.items[id]`（指针），主 goroutine 在 `worker_test.go:575` 直接
访问该指针的 `.Status` 字段，不持锁。两者并发读写同一内存地址。

**修复方向**：`get()` 方法内部应持 `f.mu` 锁并返回值拷贝，而非指
针。或者主 goroutine 的轮询改用带锁的 `get()` 方法（已有，但路径
575 没有调用它——它调用的是 `f.outbox.get(item.ID).Status`，而
`get()` 确实持锁，问题在于 `ClaimNext` 修改的是 `f.items[id]`
的字段值，而 `get()` 返回的是 `*f.items[id]`（同一指针），读写
仍并发）。正确修复：`get()` 返回副本（`copy := *it; return &copy`）。

---

## 测试代码质量问题

### Q1 t.Helper() 覆盖不完整

- `retry_test.go`、`render/render_test.go`、`tg/client_test.go` 无
  顶层 helper 函数，所有断言写在 `TestXxx` 内，可接受。
- 但 `worker_test.go` 中 `newWorkerFixture` 调用了 `t.Helper()`，
  `enqueue` 也调用了，良好。
- 总体：`t.Helper()` 在 helper 函数中使用得当，无明显缺失。

### Q2 TestWorkerRunProcessesItem 违反 spec §6 纪律

- spec §6 明确要求：等待异步生效用 `eventually` 助手，不用 sleep。
  `runtime/run_test.go` 自己实现了 `eventually`，但
  `worker_test.go:580` 用的是裸 `time.Sleep(10ms)` 轮询，与纪律
  不符，且引发了竞争（见上）。

### Q3 多个 web 测试中断言缺乏 context

- 部分 `t.Fatalf` 调用格式如 `t.Fatalf("status = %d", resp.StatusCode)`
  未说明期望值，调试时需要回读代码。
- 示例：`web/bot_test.go:99`、`web/channel_test.go:54`。
- 建议格式：`t.Fatalf("create status = %d, want 201", resp.StatusCode)`。

### Q4 TestRunEndToEnd 为单一巨型测试

- `runtime/run_test.go:290` 的 `TestRunEndToEnd` 是一个线性序列，
  覆盖 8 个步骤（admin 登录 → 创建 invite → 注册 → 创建 bot/tpl/
  ch → push → worker 消费 → 查 log）。任何一步失败，后续步骤也失
  败，错误信息指向最后失败行，定位慢。
- 这与 spec §6"测试边界"精神相悖（"端到端：主路径与 4 个失败
  路径"应拆分）。目前失败路径（disabled、dedup、bad json、404）
  都在 `web/push_test.go` 中，E2E 只覆盖主路径，可接受；但建议
  把 E2E 按阶段拆成 `TestRunBootstrapFlow`、`TestRunPushFlow` 等，
  更易定位。

### Q5 domain 包无测试文件

- `internal/domain/` 有 7 个文件，0 个 `_test.go`。domain 层含
  `FakeClock`、各常量（`OutboxPending`、`LogSent` 等）和类型断言。
  虽然逻辑极薄（基本是结构体 + 常量），spec §6 目标 100% 覆盖率，
  当前 0% 不符合目标。
- `domain/clock.go`（`FakeClock.Advance`）被多个测试使用但自身未
  测；`domain/errors.go` 的 `Is` / `As` 链路未测。

### Q6 table-driven tests 覆盖不均

- `render/render_test.go` 的 `TestEscapeMarkdownV2`、`TestEscapeHTML`
  是标准表驱动，良好。
- `auth/service_test.go:148`（`TestRegisterInviteCases`）用了子测
  试，良好。
- `tg/client_test.go` 的 `TestClassify*` 系列是独立函数而非表驱动，
  可合并为一个表驱动测试，减少重复。

---

## 加分项

1. **FakeClock 贯穿全仓**：所有 worker/cleanup/store/web 测试均注
   入 `domain.FakeClock`，完全消除了真实时间依赖，符合 spec §6
   纪律。
2. **并发 ClaimNext 测试**：`TestOutboxRepo_ClaimNext_Concurrent`（8
   goroutine 竞争 1 行）是高质量并发集成测试，直接验证 SQLite 原子
   更新语义。
3. **跨租户测试覆盖 bot/channel/dlq/log/invite**：每个资源都有至
   少一个跨租户验证，多租户隔离的信心较高。
4. **httptest.Server 用于 tg 包**：`tg/client_test.go` 用真实 HTTP
   服务器验证请求格式、路径、body，覆盖了 8 种 HTTP 响应码，测试
   质量高。
5. **eventually 助手**：`runtime/run_test.go` 实现了带 deadline 的
   轮询助手，避免了大量 `time.Sleep`，符合 spec §6。
6. **testHarness 设计**：`web/testhelpers_test.go` 的 `testHarness`
   完整装配真实 SQLite + 所有 repo + FakeClock，每个测试独立 DB
   文件，无测试间状态共享。
7. **CSRF 双向测试**：`TestCSRFTokensRoundTrip` 同时验证 header 匹
   配、缺失、篡改三条路径，安全测试意识良好。

---

## 上生产前的最小补测清单（10 条）

1. **修复 TestWorkerRunProcessesItem 数据竞争**：`fakeOutbox.get()`
   返回值拷贝而非指针；主 goroutine 轮询改用 `eventually` 而非
   裸 `time.Sleep`。（`pipeline/worker_test.go`）

2. **补 domain 包基础测试**：`FakeClock.Advance`、错误类型的
   `errors.Is` 链路、`OutboxStatus` 常量集合完整性。
   （新建 `domain/domain_test.go`）

3. **补 dedup expires_at == now 边界测试**：窗口精确为 `windowSec`
   秒，`now+windowSec` 应视为过期。
   （`store/dedup_ratelimit_test.go`）

4. **补 middleware 单测**：`CSRFCheck` 的 cookie 缺失、token 不匹配
   路径；`RequireAuth` 的 session 缺失 / 过期路径；`RequireAdmin`
   的 role=user 路径。（新建 `web/middleware/middleware_test.go`）

5. **补 Template 跨租户测试**：Template GET/PUT/DELETE 各增加一个
   跨租户用例，期望 404。（`web/template_test.go`）

6. **补 Bot/Channel 跨租户 PUT/DELETE 测试**：现有测试只验证 GET。
   （`web/bot_test.go`、`web/channel_test.go`）

7. **补 ratelimit 并发测试**：多 goroutine 同时 `Allow` 同一
   channelID，验证 `tokens` 不会被多次消耗（double-allow）。
   （`store/dedup_ratelimit_test.go`）

8. **补 worker 优雅关闭测试**：sender 阻塞期间 ctx cancel，验证
   outbox 行最终不停留在 in_flight。
   （`pipeline/worker_test.go`）

9. **补 runtime ReclaimInFlight 启动测试**：预埋一条
   `in_flight + claimed_at=2h前` 的 outbox 行，`startRuntime` 后
   断言该行变为 `retry`。（`runtime/run_test.go`）

10. **pipeline 覆盖率补全至 85%**：重点是 `worker.go` 中
    `markRetryOrDead`（unknown error 路径）和 `decodePayload`
    （malformed JSON 路径）的直接测试，目前只被间接覆盖。
    （`pipeline/worker_test.go`）
