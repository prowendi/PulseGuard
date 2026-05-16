# PulseGuard 代码质量审核 Round 2

日期：2026-05-17（V3 新增内容）
范围：internal/scripting / cmdrun / store(command/subscriber/channel) /
platform / web(command/subscriber/channel) / runtime / assets.go /
migrations 0003-0004 / web/templates

## 摘要

- 总体评级：A-
- CRITICAL：1
- HIGH：2
- MEDIUM：5
- LOW：4
- 加分项：8

---

## 阻塞性问题（合并前必须修复）

### C-B1 [CRITICAL] apiCommandTest 泄露原始 Starlark 错误

- 文件：`internal/web/command_api.go:241`
- 现象：default 分支 `out.Error = runErr.Error()` 直接把 Starlark
  EvalError 写入 API 响应，包含堆栈、文件路径、行号
- 影响：信息泄露，让恶意租户得以 fingerprint 内部 sandbox 实现
- 修复：与 listener `friendlyDispatchError` 对齐，default 改 opaque
  + Logger.Warn 内部细节

---

## HIGH 问题

### C-H1 cmdrun.Dispatcher 全文件零 slog

- 文件：`internal/cmdrun/dispatcher.go:82-104`
- 现象：Recorder.Upsert 失败被 `_ =` 静默；Executor 失败无 log
- 影响：生产盲区，磁盘满/schema 不一致/脚本异常运维无法快速定位
- 修复：New 注入 *slog.Logger，关键失败分支打 Warn（带 tenant_id +
  command_id）

### C-H2 atomicBool 命名误导（实为 mutex）

- 文件：`internal/scripting/starlark.go:263-269`
- 现象：类型 atomicBool + 注释"lightweight, lock-free flag"，但
  实现用 sync.Mutex；性能浪费 + 名字误导
- 修复：换 sync/atomic.Bool（Go 1.19+），删自定义 atomicBool

---

## MEDIUM 问题

### C-M1 channel_repo.ListByTenant N+1 查询

- 文件：`internal/store/channel_repo.go:251-275`
- 现象：先查全量 channels 然后逐行 loadBindings；50 个 channel
  产生 51 条 SQL
- 修复：单次 `SELECT channel_templates WHERE channel_id IN (?)` +
  Go 端 group-by；或一次 JOIN

### C-M2 buildBindings / buildUIBindings 重复

- 文件：`internal/web/channel_api.go:351-397` vs
  `internal/web/channel_bindings.go:15-58`
- 现象：两个函数业务验证完全相同（dedup、ownership、default 提升），
  ~80 行重复
- 修复：抽出 `validateAndBuildBindings(ctx, deps, tenantID, ...)`
  纯业务函数；API/UI 各自翻译错误展示

### C-M3 writeJSON(w, 204, nil) 语义矛盾

- 文件：`internal/web/command_api.go:194`,
  `internal/web/subscriber_api.go:67`
- 现象：HTTP 204 No Content 本应无 body；与
  `channel_api.go:304` `w.WriteHeader(http.StatusNoContent)` 不一致
- 修复：统一 `w.WriteHeader(http.StatusNoContent)` 无 body

### C-M4 Executor.HTTP 跨租户共享

- 文件：`internal/runtime/run.go:223-224`
- 现象：全局 HTTPClient 注入到全租户共用 Executor；策略无法 per-tenant
  定制
- 现状：MVP 可接受；标记 tech-debt

### C-M5 Starlark 沙箱 GlobalReassign:false 用户体验

- 文件：`internal/scripting/starlark.go:137-143`
- 现象：顶层 `counter = 0; counter += 1` 会失败，用户文档/demo
  未提及
- 修复：commandDemos 或 UI 帮助里加说明"顶层变量一旦赋值不可修改"

---

## LOW 问题

### C-L1 命名 SubscriberRecorder vs SubscriberRepo

- 文件：`internal/cmdrun/dispatcher.go:32` vs
  `internal/domain/repo.go:141`
- 现状：cmdrun 用 Recorder 名字；其他都叫 Repo；ISP 设计正确，命名
  轻微认知负担
- 修复：保持现状或改 SubscriberUpserter；非阻塞

### C-L2 printSink newline 边界

- 文件：`internal/scripting/starlark.go:226-239`
- 现象：remaining=1 且 buf 非空时，写 newline 后 chunk 截到 0；
  output 多一空行（不影响安全）
- 修复：短路判断 `remaining <= 1 && buf.Len() > 0`

### C-L3 listener_dispatch_test.go 硬 sleep 50ms

- 文件：`internal/platform/telegram/listener_dispatch_test.go:129`
- 现象：CI 负载高时 flaky
- 修复：换 eventually helper（同文件已有）

### C-L4 commands.html / subscribers.html 缺渲染测试

- 文件：`web/templates/commands.html`、`subscribers.html`
- 现状：UI handler 调 Render 但无 handler-level 测试
- 修复：参照 bot_test.go / channel_test.go 增加 200 OK + HTML
  关键元素断言

---

## 加分项

1. scripting 包 zero-dep（只依赖 starlark + 标准库）
2. SSRF guard 覆盖 6 大类（loopback/link-local/private/multicast/
   unspecified/metadata）+ DNS rebinding 通过 pre-resolve + stub-able
   lookupIPs 堵
3. platform.Manager 双重锁检查防 closed-during-swap，entry.done
   channel + defer close，panic recovery 单 listener panic 不杀
   全进程
4. cmdrun.Dispatcher 接口窄到只 Upsert（ISP 满分），单测全 fake
5. migration 0003 INSERT...SELECT 回填 + DROP COLUMN 不丢数据；
   0004 ON DELETE CASCADE 清理 subscribers
6. friendlyDispatchError 永不泄露内部错误到聊天
7. 表驱动 SSRF 测试 + eventually helper + cascading delete
8. embed FS 显式枚举防敏感文件意外暴露

---

## 优先级 1 重构清单

| # | Issue | 文件 | 预估 |
|---|-------|------|------|
| 1 | apiCommandTest 错误泄露 | command_api.go:241 | 10 min |
| 2 | Dispatcher 加 slog | cmdrun/dispatcher.go | 30 min |
| 3 | atomicBool → atomic.Bool | starlark.go:263-269 | 5 min |
| 4 | buildBindings DRY | channel_api.go + channel_bindings.go | 45 min |
| 5 | 204 NoContent 一致性 | command_api.go:194 + subscriber_api.go:67 | 5 min |

---

## 命名一致性

| 概念 | domain | store | cmdrun | web |
|------|--------|-------|--------|-----|
| 命令 | Command | CommandRepo | CommandResolver | commandView |
| 订阅 | Subscriber | SubscriberRepo | SubscriberRecorder | subscriberView |
| 绑定 | ChannelTemplate | (inline) | N/A | channelTemplateBindingView |

cmdrun 层 Resolver/Recorder ISP 窄接口设计正确；命名差异可接受。

---

## 评价

**REQUEST CHANGES** — 1 CRITICAL（信息泄露）+ 2 HIGH（日志盲区 +
误导类型名）合并前必修。修复后评级可升至 A。
