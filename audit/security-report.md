# PulseGuard 安全审计报告

日期：2026-05-17
范围：internal/、cmd/、web/、scripts/、assets.go 全量
评级：MEDIUM（设计扎实，未发现可直接利用的 Critical 漏洞）

## 摘要

- Critical：0
- HIGH：2
- MEDIUM：5
- LOW：4

---

## HIGH 发现（必须修复）

### S-H1 模板 SSTI 风险 + 渲染资源失控

- 文件：`internal/render/render.go:43`
- 触发：任一已认证租户都可创建/更新模板
- 影响：信息泄露（通过模板内省）；模板执行不限时/不限输出，恶意
  模板如 `{{range $i := .}}{{range $j := .}}` 可触发二次爆炸导致
  worker goroutine 饥饿
- 现状：用了 `text/template`，funcMap 锁死在 5 个安全函数，但
  `text/template` 仍暴露 `call` 内建，未来传入更复杂的 payload 即可
  能反射调用方法；`internal/web/template_api.go:247` 的预解析只校验
  语法，不限执行成本
- 修复建议：
  ```go
  func Render(ctx context.Context, tpl *domain.Template,
      payload map[string]any) (string, error) {
      t, err := template.New(name).
          Funcs(FuncMap).
          Option("missingkey=error").
          Parse(tpl.Body)
      if err != nil { return "", err }
      var buf limitedBuffer // cap 64KB
      ctx2, cancel := context.WithTimeout(ctx, 2*time.Second)
      defer cancel()
      done := make(chan error, 1)
      go func() { done <- t.Execute(&buf, payload) }()
      select {
      case err := <-done: return buf.String(), err
      case <-ctx2.Done(): return "", ctx2.Err()
      }
  }
  ```

### S-H2 内部错误信息泄露给外部

- 文件：`internal/web/push_api.go:33`、`push_api.go:68`、
  `validation.go:47`、`auth_api.go:171`
- 触发：远程，部分端点无需认证（push）
- 影响：暴露数据库 schema 片段、内部路径、依赖错误字符串，便于
  攻击者画图
- 现状：多处 handler 直接把 `err.Error()` 塞进 5xx JSON 响应
- 修复：日志打全错，对客户端只返 opaque message + request_id：
  ```go
  deps.Logger.Error("push channel lookup",
      "err", err, "path", r.URL.Path)
  writeError(w, r, 500, "INTERNAL",
      "internal error; see request_id for details")
  ```

---

## MEDIUM 发现

### S-M1 缺安全响应头

- 文件：`internal/web/server.go`（整体路由配置）
- 影响：点击劫持、MIME 嗅探、Referer 泄露
- 现状：未设 `X-Frame-Options` / `X-Content-Type-Options` /
  `Referrer-Policy` / `Content-Security-Policy` / `HSTS`
- 修复：
  ```go
  r.Use(func(next http.Handler) http.Handler {
      return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          w.Header().Set("X-Frame-Options", "DENY")
          w.Header().Set("X-Content-Type-Options", "nosniff")
          w.Header().Set("Referrer-Policy",
              "strict-origin-when-cross-origin")
          w.Header().Set("Content-Security-Policy",
              "default-src 'self'; script-src 'self'; "+
              "style-src 'self' 'unsafe-inline'")
          next.ServeHTTP(w, r)
      })
  })
  ```

### S-M2 CSRF token 未与 session 绑定

- 文件：`internal/web/csrf.go:19-30`
- 影响：在子域名场景下可被 cookie 注入绕过
- 现状：CSRF 是独立随机值，没有与 session_id 做 HMAC 绑定
- 修复：用 HMAC(session_id, random)：
  ```go
  func IssueCSRF(w http.ResponseWriter, sessionID string,
      secret []byte, secure bool) string {
      nonce := randomURLToken(16)
      mac := hmac.New(sha256.New, secret)
      mac.Write([]byte(sessionID + "|" + nonce))
      sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
      // ... 设 cookie 为 nonce+"."+sig
  }
  ```

### S-M3 embed 用 `all:web` 范围过宽

- 文件：`assets.go:13`
- 影响：dotfile / 编辑器临时文件 / .env 误置即可被公开下载
- 现状：`//go:embed all:web` 包含所有文件（含 dotfile）
- 修复：改用明确 glob：
  ```go
  //go:embed web/templates/*.html web/templates/**/*.html
  //go:embed web/static/htmx.min.js web/static/app.css web/static/app.js
  var WebFS embed.FS
  ```

### S-M4 push 端点无 body 大小上限

- 文件：`internal/web/push_api.go:44-48`
- 影响：内存耗尽 DoS；SQLite 膨胀（payload 作为 TEXT 入库）
- 现状：`json.NewDecoder(r.Body).Decode(&payload)` 无 MaxBytesReader
- 修复：
  ```go
  const maxPushBody = 256 << 10  // 256KB
  limited := http.MaxBytesReader(w, r.Body, maxPushBody)
  dec := json.NewDecoder(limited)
  ```

### S-M5 config.example.yaml 含已知弱 master_key

- 文件：`config.example.yaml:23`（base64 32 个 `a`）
- 影响：运维如果不替换示例值，所有租户 bot_token 在同一已知 key 下
- 修复：启动时拒绝已知弱 key：
  ```go
  weak := bytes.Repeat([]byte{0x61}, 32)
  if bytes.Equal(key, weak) {
      return nil, errors.New(
          "master_key_b64 is the example value; generate a real key")
  }
  ```

---

## LOW 发现 / 加固建议

### S-L1 登录 timing 侧信道

- 文件：`internal/auth/service.go:117-129`
- 现状：邮箱不存在直接返回（无 bcrypt 比较），邮箱存在时跑 bcrypt
  ~200ms，时间差可枚举邮箱
- 修复：邮箱未命中时跑一次哑 bcrypt：
  ```go
  var dummyHash, _ = bcrypt.GenerateFromPassword(
      []byte("timing-pad"), bcrypt.DefaultCost)
  _ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
  return nil, domain.ErrUnauthorized
  ```

### S-L2 session cookie 未用 `__Host-` 前缀

- 文件：`internal/web/auth_api.go:137-145`
- 影响：缺纵深防御；`__Host-` 强制 Secure + Path=/ + 无 Domain
- 修复：在 cookie_secure=true 时改名为 `__Host-psg_session`

### S-L3 invite 批量生成无全局速率限制

- 文件：`internal/web/invite_api.go:131-139`
- 现状：单次上限 100；无 admin/day 累计上限
- 修复：滑动窗口 500/day/admin

### S-L4 登录/注册端点无专用速率限制

- 文件：`internal/web/server.go:42-46`、
  `internal/web/middleware/ratelimit.go`
- 现状：全局 100 req/s/IP 对暴力撞库太宽
- 修复：login 单独 5/min/IP 或 5/min/email

---

## 已正确实施的安全控制

1. bcrypt 密码哈希 cost=10 可配 — `internal/auth/hash.go`
2. AES-256-GCM bot_token 加密 + 每次随机 nonce —
   `internal/store/crypto.go`
3. session cookie HttpOnly + Secure(可配) + SameSite=Lax —
   `internal/web/auth_api.go:137-145`
4. CSRF double-submit + 常量时间比较 —
   `internal/web/middleware/csrf.go:45`、`internal/web/csrf.go:70`
5. 多租户隔离强制 — 所有 GetByID/ListByTenant/Delete 从 ctx 取
   tenantID，禁止用户输入
6. bot_token 永不回显 API（仅末 4 位）—
   `internal/web/bot_api.go:26-28`
7. DLQ replay 仅限本租户 —
   `internal/store/deadletter_repo.go:98` `WHERE id=? AND tenant_id=?`
8. push_token 24B crypto/rand → 192-bit 熵 —
   `internal/web/channel_api.go:288-294`
9. 全 SQL 用 `?` 占位符，无字符串拼接
10. 登出真删 DB 行 — `internal/store/session_repo.go:70`
11. 敏感字段日志脱敏 — `internal/logging/logger.go:11-20`
12. push 限流 token bucket 在通道级 + 全局 IP
13. admin-only 路由套 RequireAdmin 中间件 —
    `internal/web/routes.go:33-35`
14. html/template 自动转义 UI XSS — `internal/web/render.go:38`
15. 外键 ownership 校验 channel —
    `internal/web/channel_api.go:266-284`
16. invite code 24B crypto/rand → 192-bit 熵 —
    `internal/auth/invite.go:22`

---

## 渗透测试建议

1. **push token 暴力枚举** — 高速 POST 随机 token，验证 IP 限流
   生效，且 valid/invalid 响应时长无差
2. **模板 SSTI 利用** — 创建 `{{printf "%v" .}}` + 深层 range
   嵌套，观察 worker 内存与是否能 OOM
3. **跨租户资源绑定** — 创建 channel 引用其他租户的 bot_id /
   template_id，验证返 400 VALIDATION 而非静默成功
4. **登录 timing oracle** — 测 valid-email-wrong-password vs
   invalid-email 时长差，确认 < 50ms
5. **超大 push body** — POST 100MB JSON，确认服务端 prompt 拒绝
   不 OOM

---

总结：未发现可直接利用 Critical；2 HIGH（模板注入 + 错误泄露）必修；
5 MEDIUM 中 M1 安全头 / M4 body 限大小 / M5 拒绝示例 key 上线前必修。
