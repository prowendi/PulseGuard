# PulseGuard V3 Round 2 测试覆盖审计报告

**审计日期**: 2026-05-17
**审计范围**: V3 新增包（scripting / cmdrun / platform / platform/telegram）及全套覆盖率扫描
**测试命令**: `go test ./... -cover -count=1` + `go test ./... -race -count=1`

---

## 摘要

| 维度 | 结论 |
|------|------|
| 全套测试状态 | 全部通过，零失败 |
| Race Detector | 全部通过，无数据竞争 |
| 整体健康度 | 良好，但 web 层和 middleware 层存在明显覆盖空洞 |
| 上生产前最小补测 | 6 条高优先级缺口需补充测试 |

---

## 覆盖率明细

| 包 | 覆盖率 | 评级 |
|----|--------|------|
| `internal/render` | 93.1% | 优秀 |
| `internal/platform` | 89.2% | 良好 |
| `internal/cmdrun` | 87.9% | 良好 |
| `internal/tg` | 82.2% | 良好 |
| `internal/bootstrap` | 82.4% | 良好 |
| `internal/platform/telegram` | 83.3% | 良好 |
| `internal/runtime` | 77.9% | 可接受 |
| `internal/scripting` | 75.0%–77.1% | 可接受（注意缺口） |
| `internal/store` | 76.6% | 可接受 |
| `internal/pipeline` | 73.9% | 可接受 |
| `internal/auth` | 71.0% | 偏低 |
| `internal/logging` | 69.2% | 偏低 |
| `internal/config` | 58.1% | 偏低 |
| `internal/web` | 51.3% | 偏低（关键业务层） |
| `internal/web/middleware` | 16.1% | 严重不足 |
| `internal/domain` | 0%（无测试文件） | — |
| `cmd/pulseguard` | 0% | 可接受（main 入口） |

---

## 高优先级缺口

### H1. HTTP body cap 截断时响应的行为未验证（scripting/http_module.go）

**文件**: `internal/scripting/http_module.go:162–168`

`TestHTTP_ResponseBodyCapped` 只验证了 `len(body) == MaxBodyBytes`（即截断到 4096 字节），但没有验证：
- 截断后连接是否被正确关闭（`io.LimitReader` + `resp.Body.Close()` 组合在慢响应流中的行为）
- 当 `MaxBodyBytes <= 0` 时的回落路径（第 163–165 行的 `if cap <= 0` 分支）

该路径覆盖率缺口会掩盖潜在的内存放大路径。

**风险**: 中高（安全相关）

---

### H2. SSRF：HTTP redirect 跟随未测试（scripting/http_module.go）

**文件**: `internal/scripting/http_module.go:155`（`m.c.BaseClient.Do(req)`）

`http.Client` 默认跟随重定向（最多 10 跳）。当攻击者控制的外部服务器返回 `301 → http://10.0.0.1/` 时，`CheckURL` 只在初次请求前检查目标 URL，redirect 目标完全未检查。代码中没有设置 `CheckRedirect` 策略，`NewHTTPClient` 也未禁止 redirect。

当前 `TestHTTP_SSRF_*` 系列测试全部针对直接 IP 字面量，无一测试 redirect 跟随场景。

**文件引用**: `internal/scripting/http_module.go:39–46`（`NewHTTPClient` 未设 `CheckRedirect`）
**风险**: 高（SSRF 绕过）

---

### H3. SSRF：IPv6 私网 fc00::/7（fd00::* ULA）未在 http 模块层测试

**文件**: `internal/scripting/http_module_test.go`

`TestIsBlockedIP_DenyTable` 中有 `fc00::1` 的 isBlockedIP 单元测试，但 `TestHTTP_SSRF_*` 系列中没有对应的 `http.get("http://[fc00::1]/")` 端到端路径测试。`isBlockedIP` 依赖 `net.IP.IsPrivate()`，Go 1.17+ 的 `IsPrivate()` 确实覆盖 fc00::/7，但端到端集成测试缺失意味着如果 `CheckURL` 的 IPv6 字面量解析发生回归，没有测试会捕获它。

**文件引用**: `internal/scripting/ssrf_test.go:26`（仅 isBlockedIP 层）；`internal/scripting/http_module_test.go`（缺失端到端）
**风险**: 高（SSRF 安全边界）

---

### H4. Dispatcher 跨租户拒绝路径未直接覆盖（cmdrun/dispatcher.go）

**文件**: `internal/cmdrun/dispatcher.go:108–122`（`resolve` 方法）

`fakeResolver` 的实现是静态返回 `(cmd, err)` 对，没有按 botID 做租户作用域检查。因此测试 `TestDispatcher_UnknownCommandSkips` 和 `TestDispatcher_TriesBothSlashAndBareName` 虽然覆盖了 `ErrNotFound` 路径，但没有模拟"bot 属于租户 A，command 属于租户 B，resolver 通过 JOIN 返回 ErrNotFound"的真实跨租户拒绝场景。

真实的跨租户拒绝由 `store.CommandRepo.GetByBotAndName` 的 SQL JOIN 保证（`TestCommandRepo_GetByBotAndName_CrossTenant` 有覆盖），但 dispatcher 层本身没有一个测试验证"当 resolver 正确拒绝跨租户请求时 dispatcher 的行为"。

**文件引用**: `internal/cmdrun/dispatcher.go:69–76`；`internal/store/command_subscriber_test.go:136–156`
**风险**: 中高（租户隔离）

---

### H5. 多模板 channel：`_template_id` 命中后 worker 路径未测试（pipeline/worker.go）

**文件**: `internal/pipeline/worker.go:395–425`（`pickTemplateID`）

`pickTemplateID` 有三条路径：
1. payload 中有 `_template_id` 且 channel 绑定了该 ID → 使用指定模板（**无测试**）
2. payload 中有 `_template`（字符串名称）→ 直接跳过（`_ = v`，作为注释说明是 best-effort）（**无测试**）
3. 使用 channel 默认模板 → 所有 worker 测试均走此路径

`worker_test.go` 中所有 fixture 只建了单模板 channel，`PayloadJSON` 从未包含 `_template_id` 字段。路径 1 对于多模板 push 是核心功能，路径 2 的 no-op 行为应有显式注释测试避免误解。

**文件引用**: `internal/pipeline/worker.go:400–418`；`internal/pipeline/worker_test.go`（全文无 `_template_id`）
**风险**: 高（多模板核心功能）

---

### H6. 多模板 default 不存在时的回退路径（pipeline/worker.go + domain/channel.go）

**文件**: `internal/domain/channel.go:45–55`（`DefaultTemplateID`）；`internal/pipeline/worker.go:141–145`

当 channel 的所有 `ChannelTemplate` 条目均为 `IsDefault: false` 时（即数据库约束被绕过或 `ReplaceTemplates` 在事务外崩溃），`DefaultTemplateID()` 返回 0，worker 进入 `dlq(ctx, item, "", "channel has no template binding")` 分支。

没有任何测试覆盖此分支。`worker_test.go` 中所有 fixture 的 `IsDefault: true` 是硬编码的。

**文件引用**: `internal/pipeline/worker.go:141–145`；`internal/pipeline/worker_test.go:309`
**风险**: 中高（静默 DLQ，难以排查）

---

## 中优先级缺口

### M1. Starlark timeout：compile 阶段超时路径未独立验证

**文件**: `internal/scripting/starlark.go:144–150`

`TestExecute_TimeoutInfiniteLoop` 测试的是运行阶段（`starlark.Call` 内）超时。compile 阶段（`ExecFileOptions`）也会触发 `thread.Cancel`，但没有专门测试验证"在编译期间 context 超时"的行为，特别是 `classifyErr` 中 `strings.Contains(msg, "cancelled")` 的路径（第 208 行）。

**文件引用**: `internal/scripting/starlark.go:144–150, 207–210`
**风险**: 中

---

### M2. web/middleware 整体覆盖率仅 16.1%

**文件**: `internal/web/middleware/`

除 `logger_test.go`（2 个测试）外，以下中间件无测试：
- `auth.go`（会话验证中间件）
- `csrf.go`（CSRF 双 token 验证）
- `ctx.go`（租户 context 注入）
- `recover.go`（panic recover）
- `secureheaders.go`（安全响应头）
- `ratelimit.go`（IP 速率限制）

`TestSecureHeadersOnEveryResponse` 在 `web/server_test.go` 中通过集成测试间接覆盖了 secureheaders，但 recover、ratelimit、csrf 中间件均无独立单元测试。

**文件引用**: `internal/web/middleware/*.go`
**风险**: 中（安全相关中间件的行为未验证）

---

### M3. command API：`/test` 端点无 HTTP 层测试

**文件**: `internal/web/command_api.go:202–244`（`apiCommandTest`）

`POST /api/v1/commands/{id}/test` 端点的以下路径无测试：
- `ScriptExec == nil` 时返回 503
- 执行成功返回 output + return
- ErrTimeout / ErrUnsafeHost / ErrMissingHandle 映射到友好消息
- 跨租户命令 ID 访问（GetByID 租户隔离）

**文件引用**: `internal/web/command_api.go:202–244`；无对应测试文件条目
**风险**: 中

---

### M4. push API：trailing garbage JSON 路径未测试

**文件**: `internal/web/push_api.go:68–71`

`dec.More()` 检测 trailing content 并返回 400，但 `push_test.go` 中没有测试发送 `{"k":1}garbage` 形式的请求体。

**文件引用**: `internal/web/push_api.go:68–71`
**风险**: 低中

---

### M5. channel update：显式传空 template_ids 清空绑定路径未测试

**文件**: `internal/web/channel_api.go:255–268`

当 PUT 请求中 `template_ids` 为明确的 `[]`（空数组，非 null）时，代码执行 `ReplaceTemplates(ctx, tenantID, ch.ID, nil)`，清空所有绑定。这是一个破坏性操作，没有对应测试验证该行为。

**文件引用**: `internal/web/channel_api.go:255–268`
**风险**: 中（数据安全）

---

### M6. Listener Manager 高并发 Start/Stop 缺乏并发压力测试

**文件**: `internal/platform/manager.go:69–128`

`TestManager_StartIdempotentReplacesListener` 验证了顺序替换，但没有并发场景：10 个 goroutine 同时对同一 botID 调用 `Start`，或者 `Start` 和 `Shutdown` 并发调用。代码中有复杂的 mutex 解锁-重新加锁序列（第 91–100 行），这在高并发下可能存在 TOCTOU 问题（虽然 race detector 未发现，但未经压力测试）。

**文件引用**: `internal/platform/manager.go:91–100`；`internal/platform/manager_test.go`
**风险**: 中

---

## Flaky 风险

### F1. `TestListener_CustomCommandWithoutDispatcherSilent` - 固定 sleep

**文件**: `internal/platform/telegram/listener_dispatch_test.go:226`

```go
time.Sleep(150 * time.Millisecond)
```

该测试用 150ms sleep 来断言"没有 reply 被发送"。在 CI 高负载场景下，若进程调度延迟超过 150ms，测试可能在消息被处理之前就检查状态，导致误判通过（false negative）。正确做法是等待至少一次 getUpdates 轮询完成后再断言无 reply。

**建议修复**: 用 `eventually(..., func() bool { return atomic.LoadInt32(&srv.getUpdatesCalls) >= 2 })` 替代 sleep，参照同文件 `TestListener_NewChatMembersWithoutSelfIgnored` 的做法。

---

### F2. `TestWorkerRunRespectsContext` - 固定 sleep

**文件**: `internal/pipeline/worker_test.go:599`

```go
time.Sleep(20 * time.Millisecond) // let the loop spin a bit
```

在极低负载的测试机器上，20ms 可能不够让 Run 循环进入 `select` 等待，导致 cancel 在循环甚至开始之前就被调用。概率较低，但在 CI 中是潜在的间歇性失败源。

**建议修复**: 改为等待一个通道信号确认 Run 已进入等待状态，或接受当前行为（风险极低）。

---

### F3. `TestListener_NonFatalErrorRetries` - 10s 超时依赖 `pollErrorBackoff`

**文件**: `internal/platform/telegram/listener_test.go:466`

```go
eventually(t, 10*time.Second, func() bool {
    return len(srv.sentSnapshot()) >= 1
})
```

该测试依赖 5s 的 `pollErrorBackoff`（`listener.go:87`），整个测试最长等 10s。若机器慢或 CI 负载高，5s 重试间隔加上两次 poll 开销可能逼近 10s 上限，导致偶发超时失败。

**建议**: 考虑将 `pollErrorBackoff` 注入为 Listener 的可配置字段，测试时设为 100ms。

---

## 测试代码质量

### Q1. fakeResolver 不按 botID 做作用域（cmdrun）

`internal/cmdrun/dispatcher_test.go:17–31`

`fakeResolver.GetByBotAndName` 忽略 `botID` 参数，仅返回静态的 `(cmd, err)`。这使得测试无法验证 dispatcher 在"不同 botID 对应不同租户"场景下的行为。建议在 fakeResolver 中增加按 `botID` 映射的支持，以便覆盖跨租户拒绝路径（见 H4）。

---

### Q2. worker 测试所有 fixture 单模板，无法覆盖 pickTemplateID 主要分支

`internal/pipeline/worker_test.go:308–311`

所有 fixture 的 channel 只有一个 `IsDefault: true` 的模板，`PayloadJSON` 也从不包含 `_template_id`。`pickTemplateID` 的第 1 条路径（按 ID 明确指定）是多模板 push 的核心路径，在测试中完全不可见。参见 H5。

---

### Q3. 表驱动测试不充分的地方

- `TestIsBlockedIP_DenyTable`（`ssrf_test.go:10`）：表驱动做得好，但缺少 `fd00::1`（fc00::/7 的 fd 子段）
- `TestCheckURL_RejectsPrivateLiteralWithoutDNS`：列表式而非表驱动，可读性降低
- `TestDispatcher_*`：所有测试均为独立函数，没有利用表驱动共享"命令名解析"场景，重复构造较多

---

### Q4. 集成测试依赖真实 SQLite，但 ingest_test.go 创建多个临时数据库文件

`internal/pipeline/ingest_test.go:101–191`

每个测试用例都调用 `t.TempDir()` 创建独立 DB，这是正确的隔离做法，但会在测试运行时产生 6 个临时 SQLite 文件（含 WAL 文件）。在 Windows 上 SQLite WAL 模式下，`t.Cleanup` 有时无法删除文件（文件锁未完全释放），留下垃圾文件。当前无 `db.Close()` 前的 WAL checkpoint，建议在 `t.Cleanup` 中显式执行 `PRAGMA wal_checkpoint(TRUNCATE)`。

---

## 加分项（现有测试中的亮点）

1. **`TestListener_CustomCommandDispatched`** 中文命令名 `/查询 1` 路径有端到端覆盖，验证了 `strings.Fields` 对 UTF-8 的正确切分
2. **`TestHTTP_SSRF_DNSRebindingBlocked`** 通过 stub `lookupIPs` 覆盖了 DNS rebinding 场景，设计精准
3. **`TestCleanupRunFiresEachSweepDeterministically`** 使用 TickerSource 注入彻底消除了 sleep，是 deterministic 测试的范例
4. **`TestWriteInternalDoesNotLeakErrorDetail`** 明确验证了错误不泄漏到响应体，是安全回归守护的正确做法
5. **`TestDispatcher_RecorderErrorNonFatal`** 验证了 upsert 失败不影响脚本执行，符合设计意图
6. **Manager 测试** 覆盖了 panic recover、build 失败、幂等替换、parent context cancel 等所有正常路径
7. **`TestBotRepo_Migrate0002BackfillsExistingRows`** 覆盖了迁移回填路径，是难得的迁移测试

---

## 上生产前的最小补测清单

按优先级排序，以下 6 条是最小补测集：

| 编号 | 补测内容 | 目标文件 |
|------|----------|----------|
| P1 | HTTP redirect 跟随 SSRF：让 httptest server 重定向到私网 IP，验证被拒绝 | `internal/scripting/http_module_test.go` |
| P2 | IPv6 私网 fc00::/7 在 http 模块层端到端验证 | `internal/scripting/http_module_test.go` |
| P3 | `pickTemplateID`：payload 含 `_template_id` 时 worker 选正确模板 | `internal/pipeline/worker_test.go` |
| P4 | channel 无任何 default 模板时 worker DLQ 路径 | `internal/pipeline/worker_test.go` |
| P5 | Dispatcher fakeResolver 支持按 botID 隔离，补跨租户拒绝测试 | `internal/cmdrun/dispatcher_test.go` |
| P6 | 修复 `TestListener_CustomCommandWithoutDispatcherSilent` 的固定 sleep | `internal/platform/telegram/listener_dispatch_test.go:226` |

---

*本报告为只读分析，未修改任何生产代码或测试代码。*
