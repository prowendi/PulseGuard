# PulseGuard 安全审计 Round 2

日期：2026-05-17（R2 整改后复审）
范围：V3 新增 + R2 整改后的 scripting / cmdrun / store / platform /
web / migrations / templates / vendoring

## 摘要

- Risk Level：MEDIUM
- Critical：0
- HIGH：1
- MEDIUM：3
- LOW：3
- 已正确实施：12 项

---

## HIGH 发现

### S2-H1 DNS Rebinding TOCTOU 窗口

- 文件：`internal/scripting/http_module.go:149-156`
- 触发：远程，需攻击者控制 DNS 服务器，已认证租户脚本可触发
- 影响：可访问内网/云 metadata/K8s API；命中取决于部署
- 现状：`CheckURL` 解析 IP 校验通过后，第 156 行用**原始 URL**
  构造 http.Request；`BaseClient.Do` 会再次 DNS lookup，攻击者
  DNS server 此时可改 A 记录指向内网（经典 DNS rebinding）。
  5s 超时缩小窗口但不消除
- 修复：自定义 `Transport.DialContext` 用 CheckURL 返回的已校验
  IP 直接拨号，跳过二次 DNS 解析：
  ```go
  func safeDialer(ips []net.IP) func(ctx context.Context, network, addr string) (net.Conn, error) {
      return func(ctx context.Context, network, addr string) (net.Conn, error) {
          _, port, _ := net.SplitHostPort(addr)
          return (&net.Dialer{}).DialContext(ctx, network,
              net.JoinHostPort(ips[0].String(), port))
      }
  }
  ```

---

## MEDIUM 发现

### S2-M1 Starlark 脚本代码体积无上限

- 文件：`internal/web/command_api.go:99-101`、
  `internal/store/command_repo.go:165-179`
- 触发：已认证租户
- 影响：编译大脚本耗 CPU/内存；SQLite 膨胀
- 现状：仅 TrimSpace 非空校验；body 1MiB 全用完都能存
- 修复：加 `const MaxCommandCodeBytes = 64*1024`，create/update
  超过返 400 VALIDATION

### S2-M2 Tailwind v2.2.19 已过时

- 文件：`web/static/tailwind.min.css`（commit 05a151b）
- 影响：CSS 框架直接漏洞少；体积大 2.93MB；无 SRI hash 校验
- 现状：本地 embed 非 CDN，CSP 限制外部样式；实际风险有限
- 修复：升级 v4.x + PurgeCSS tree-shake；commit 注释里记 SHA256

### S2-M3 API CSRFCheck 仅 cookie vs header，未绑定 session

- 文件：`internal/web/middleware/csrf.go:21-52`
- 触发：需要 cookie injection 能力（子域 XSS 前置）
- 影响：CSRF 保护可被 cookie tossing 绕过（针对 API 路由；UI 已 HMAC 校验）
- 现状：UI 路由的 `web.VerifyCSRF()` 调 `VerifyCSRFToken()` 校验
  HMAC；middleware/csrf.go 只 `ConstantTimeCompare`
- 修复：把 middleware CSRFCheck 改为 HMAC-aware，从 session 取
  sessionID 校验 token 绑定

---

## LOW 发现

### S2-L1 Command 名称无字符集限制

- 文件：`internal/web/command_api.go:95-97`、`validation.go:54-64`
- 现状：仅长度上限。html/template 自动转义所以无 XSS；SQL 全参数化
- 修复：限制为字母/数字/中文/下划线/连字符，禁控制字符

### S2-L2 Subscriber chatID 类型 TEXT 无格式校验

- 文件：`migrations/0004_commands_subscribers.sql:34`、
  `cmdrun/dispatcher.go:108`
- 现状：Telegram 来源是 int64.String()，攻击不可控；多平台扩展时
  需加校验
- 修复：未来加 chatID 字符集白名单

### S2-L3 error.Error() 渗入 UI flash

- 文件：`internal/web/command_ui.go:98`
- 影响：底层 SQLite 约束错误可能含表/列名
- 修复：映射 ErrConflict → "命令名已存在" 等友好消息

---

## 已正确实施

1. **Starlark 沙箱**：load 禁用，open/os/sys 不在 predeclared，
   while/recursion 禁用，步数 100M，超时 10s，测试齐
2. **SSRF 基础**：isBlockedIP 覆盖 loopback/private/link-local/
   multicast/unspecified/metadata + IPv6 fc00::/7
3. **HTTP 重定向阻断**：blockRedirect 用 http.ErrUseLastResponse，
   端到端测试齐
4. **Body 1MiB 上限**：io.LimitReader 强制
5. **CSRF**：UI 走 HMAC + session 绑定（API 待 M3）
6. **Auth/越权**：commands/subscribers/channels 路由全套 RequireAuth +
   CSRFCheck；repo 强制 tenant_id 隔离
7. **SQL 参数化**：所有查询 `?` 占位符
8. **安全响应头**：XFO/CSP/X-Content-Type-Options/HSTS 完整
9. **日志脱敏**：password/bot_token/push_token/master_key/secret/
   cookie 自动 REDACT
10. **请求体 1 MiB MaxBytesReader**
11. **密码学**：bcrypt + HMAC-SHA256 + crypto/rand + constant-time
12. **依赖审计**：chi/httprate/starlark/sqlite/yaml/bcrypt 全 latest，
    无已知 CVE；Go 1.25.1 最新

---

## 渗透测试建议

1. **DNS rebinding**：自建 DNS server TTL=0 切换公网 IP →
   169.254.169.254，starlark 调 http.get 看能否读到云 metadata
2. **CSRF cookie tossing**：子域 XSS 设 psg_csrf cookie，向
   /api/v1/commands POST 验证 middleware 是否被绕过
3. **大脚本 DoS**：900KB starlark 含深嵌 for，并发 5-10 次看内存峰值
4. **command 名 injection**：尝试 `<script>alert(1)</script>` 和
   `'); DROP TABLE`

---

## Security Checklist

- [x] 无硬编码 secret（env 注入）
- [x] 全部输入校验（MaxBytes + decodeJSON + validateName）
- [x] 注入防护（SQL ? 参数化、html/template）
- [x] Auth/授权（RequireAuth + tenant scope）
- [x] 依赖审计（无 CVE）
- [x] SSRF 基础（IP deny + scheme allow + redirect block）
- [ ] DNS rebinding TOCTOU（S2-H1 待修）
- [ ] 脚本代码大小未限（S2-M1 待修）
- [ ] API CSRF middleware 缺 HMAC 绑定（S2-M3 待修）

---

**一句话总结**：整改后无 Critical；最高优 H1 DNS rebinding 需通过
自定义 DialContext 把 CheckURL 解析的 IP 注入到连接阶段；M1+M3 一同
合并修复后可上生产。
