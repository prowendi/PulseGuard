package web

import (
	"net/http"
	"strings"
	"time"

	"github.com/wendi/pulseguard/internal/domain"
	"github.com/wendi/pulseguard/internal/platform"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// botListPage is the data wrapper for /ui/bots.
type botListPage struct {
	pageData
	Bots []botView
}

func installBotsUIRoutes(r chi.Router, deps Deps) {
	r.Get("/bots", uiBotList(deps))
	r.Post("/bots", uiBotCreate(deps))
	r.Post("/bots/{id}/update", uiBotUpdate(deps))
	r.Post("/bots/{id}/delete", uiBotDelete(deps))
}

func uiBotList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		items, err := deps.Bots.ListByTenant(r.Context(), tenant.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		now := time.Now()
		var snap map[int64]platform.BotHealth
		if deps.BotListeners != nil {
			snap = deps.BotListeners.HealthSnapshot()
		}
		views := make([]botView, 0, len(items))
		for _, b := range items {
			views = append(views, toBotViewWithHealth(b, snap[b.ID], now))
		}
		_ = Render(w, http.StatusOK, "bots-page", botListPage{
			pageData: pageData{
				Title:  "Bots",
				Tenant: tenant,
				Active: "bots",
				CSRF:   readCSRFCookie(r),
				Theme:  themeFromRequest(r),
			},
			Bots: views,
		})
	}
}

func uiBotCreate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		name := strings.TrimSpace(r.PostForm.Get("name"))
		token := strings.TrimSpace(r.PostForm.Get("bot_token"))
		desc := r.PostForm.Get("description")
		plat := strings.TrimSpace(r.PostForm.Get("platform"))
		if plat == "" {
			plat = domain.PlatformTelegram
		}
		kind := strings.TrimSpace(r.PostForm.Get("bot_kind"))
		if kind == "" {
			kind = domain.BotKindWebhook
		}
		appID := strings.TrimSpace(r.PostForm.Get("app_id"))
		appSecret := strings.TrimSpace(r.PostForm.Get("app_secret"))
		verifyToken := strings.TrimSpace(r.PostForm.Get("verify_token"))
		encryptKey := strings.TrimSpace(r.PostForm.Get("encrypt_key"))
		tenant := wmw.Tenant(r.Context())
		if !domain.IsValidPlatform(plat) {
			uiBotListWithFlash(w, r, deps, tenant, "error", "未知 platform")
			return
		}
		if !domain.IsValidBotKind(kind) {
			uiBotListWithFlash(w, r, deps, tenant, "error", "未知 bot_kind")
			return
		}
		if name == "" {
			uiBotListWithFlash(w, r, deps, tenant, "error", "name 不能为空")
			return
		}
		isApp := kind == domain.BotKindApp
		if isApp {
			if plat != domain.PlatformLark {
				uiBotListWithFlash(w, r, deps, tenant, "error", "bot_kind=app 必须搭配 platform=lark")
				return
			}
			if !botAppCredsLookValid(appID, appSecret, true) {
				uiBotListWithFlash(w, r, deps, tenant, "error",
					"请提供合法的 app_id (cli_<hex>) 和 app_secret")
				return
			}
			if encryptKey == "" {
				uiBotListWithFlash(w, r, deps, tenant, "error",
					"encrypt_key 必填（用于校验事件签名）")
				return
			}
		} else {
			if !botTokenLooksValid(plat, token) {
				uiBotListWithFlash(w, r, deps, tenant, "error", "请提供合法的 name 与 bot_token（Telegram: 数字:字母；Lark: https://open.feishu.cn/open-apis/bot/v2/hook/...）")
				return
			}
		}
		bot := &domain.Bot{
			TenantID:    tenant.ID,
			Name:        name,
			Platform:    plat,
			BotKind:     kind,
			BotToken:    token,
			Description: desc,
			AppID:       appID,
			AppSecret:   appSecret,
			VerifyToken: verifyToken,
			EncryptKey:  encryptKey,
		}
		if err := deps.Bots.Insert(r.Context(), bot); err != nil {
			uiBotListWithFlash(w, r, deps, tenant, "error", err.Error())
			return
		}
		startBotListener(deps, bot)
		if isApp {
			uiBotListWithFlash(w, r, deps, tenant, "ok",
				"Lark 应用机器人已创建。请将 POST /api/v1/lark/events 配置到 Lark 开放平台的事件订阅 URL，并把 encrypt_key 填到对应的“加密 Key”字段。")
		} else {
			uiBotListWithFlash(w, r, deps, tenant, "ok",
				"Bot 已创建。请到 Telegram 给该 bot 发送 /start，或将 bot 拉入群组，"+
					"bot 会自动回复对话的 Chat ID。把 Chat ID 填入新建 Channel 的表单。")
		}
	}
}

// uiBotUpdate handles in-place edits from the shared edit-drawer. The
// drawer carries name / description / platform always, and bot_token
// only when the operator intentionally rotates the credential (blank
// input = keep current token). This lets routine renames stay cheap:
// only token + platform changes trigger a listener restart so the
// running long-poll loop is not bounced for cosmetic edits.
//
// LB7 extension: when bot_kind=="app", the form additionally carries
// app_id / app_secret / verify_token / encrypt_key. app_secret follows
// the same "blank = keep" semantics as bot_token.
func uiBotUpdate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		_ = r.ParseForm()
		name := strings.TrimSpace(r.PostForm.Get("name"))
		desc := r.PostForm.Get("description")
		plat := strings.TrimSpace(r.PostForm.Get("platform"))
		if plat == "" {
			plat = domain.PlatformTelegram
		}
		kind := strings.TrimSpace(r.PostForm.Get("bot_kind"))
		if kind == "" {
			kind = domain.BotKindWebhook
		}
		newToken := strings.TrimSpace(r.PostForm.Get("bot_token"))
		appID := strings.TrimSpace(r.PostForm.Get("app_id"))
		appSecret := strings.TrimSpace(r.PostForm.Get("app_secret"))
		verifyToken := strings.TrimSpace(r.PostForm.Get("verify_token"))
		encryptKey := strings.TrimSpace(r.PostForm.Get("encrypt_key"))
		tenant := wmw.Tenant(r.Context())
		if name == "" {
			uiBotListWithFlash(w, r, deps, tenant, "error", "name 不能为空")
			return
		}
		if !domain.IsValidPlatform(plat) {
			uiBotListWithFlash(w, r, deps, tenant, "error", "未知 platform")
			return
		}
		if !domain.IsValidBotKind(kind) {
			uiBotListWithFlash(w, r, deps, tenant, "error", "未知 bot_kind")
			return
		}
		// Validate the token format only when the operator actually
		// typed something. Blank means "keep existing". The platform
		// supplied in this form drives the regex choice; if the
		// operator is switching platforms (Telegram ↔ Lark) the new
		// token must match the *new* platform's shape. App-kind rows
		// have no operator-supplied token (the store derives it) so
		// the check is skipped entirely.
		if kind != domain.BotKindApp && newToken != "" && !botTokenLooksValid(plat, newToken) {
			uiBotListWithFlash(w, r, deps, tenant, "error", "bot_token 格式不正确（Telegram: 数字:字母；Lark: https://open.feishu.cn/open-apis/bot/v2/hook/...）")
			return
		}
		bot, err := deps.Bots.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			uiBotListWithFlash(w, r, deps, tenant, "error", "bot 不存在或不属于当前租户")
			return
		}
		tokenChanged := false
		platformChanged := bot.Platform != plat
		kindChanged := bot.BotKind != kind
		if kind != domain.BotKindApp && newToken != "" && newToken != bot.BotToken {
			bot.BotToken = newToken
			tokenChanged = true
		}
		bot.Name = name
		bot.Description = desc
		bot.Platform = plat
		bot.BotKind = kind
		if kind == domain.BotKindApp {
			if plat != domain.PlatformLark {
				uiBotListWithFlash(w, r, deps, tenant, "error", "bot_kind=app 必须搭配 platform=lark")
				return
			}
			if appID != "" {
				bot.AppID = appID
			}
			// AppSecret: blank means keep. Trim+assign only when set.
			if appSecret != "" {
				bot.AppSecret = appSecret
			}
			if verifyToken != "" {
				bot.VerifyToken = verifyToken
			}
			if encryptKey != "" {
				bot.EncryptKey = encryptKey
			}
			// Sanity-check the final state: app_id must match the
			// pattern; app_secret may legitimately be empty if it
			// was preserved on disk.
			needSecret := bot.AppSecret == ""
			if !botAppCredsLookValid(bot.AppID, bot.AppSecret, needSecret) {
				uiBotListWithFlash(w, r, deps, tenant, "error",
					"app_id 必须形如 cli_<hex>，首次创建时 app_secret 必填")
				return
			}
		}
		if err := deps.Bots.Update(r.Context(), bot); err != nil {
			uiBotListWithFlash(w, r, deps, tenant, "error", err.Error())
			return
		}
		// Only bounce the long-poll loop when the credentials it speaks
		// or the platform binding changed. Cosmetic edits (name /
		// description) leave the listener untouched. A disabled bot has
		// no listener to restart — restartBotListener degrades to a
		// Stop + Start where Start is a no-op for !Enabled bots inside
		// the platform manager (the runtime gates that itself).
		if tokenChanged || platformChanged || kindChanged {
			restartBotListener(deps, bot)
		}
		uiBotListWithFlash(w, r, deps, tenant, "ok", "Bot 已更新。")
	}
}

func uiBotDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r, deps.csrfSecret()) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		_ = deps.Bots.Delete(r.Context(), tenant.ID, id)
		stopBotListener(deps, id)
		http.Redirect(w, r, "/ui/bots", http.StatusSeeOther)
	}
}

func uiBotListWithFlash(w http.ResponseWriter, r *http.Request, deps Deps, tenant *domain.Tenant, level, msg string) {
	items, _ := deps.Bots.ListByTenant(r.Context(), tenant.ID)
	now := time.Now()
	var snap map[int64]platform.BotHealth
	if deps.BotListeners != nil {
		snap = deps.BotListeners.HealthSnapshot()
	}
	views := make([]botView, 0, len(items))
	for _, b := range items {
		views = append(views, toBotViewWithHealth(b, snap[b.ID], now))
	}
	_ = Render(w, http.StatusOK, "bots-page", botListPage{
		pageData: pageData{
			Title:  "Bots",
			Tenant: tenant,
			Active: "bots",
			CSRF:   readCSRFCookie(r),
			Flash:  &flash{Level: level, Message: msg},
			Theme:  themeFromRequest(r),
		},
		Bots: views,
	})
}
