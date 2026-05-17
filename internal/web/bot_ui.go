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
		platform := strings.TrimSpace(r.PostForm.Get("platform"))
		if platform == "" {
			platform = domain.PlatformTelegram
		}
		tenant := wmw.Tenant(r.Context())
		if !domain.IsValidPlatform(platform) {
			uiBotListWithFlash(w, r, deps, tenant, "error", "未知 platform")
			return
		}
		if name == "" || !botTokenPattern.MatchString(token) {
			uiBotListWithFlash(w, r, deps, tenant, "error", "请提供合法的 name 与 bot_token")
			return
		}
		bot := &domain.Bot{
			TenantID: tenant.ID, Name: name, Platform: platform, BotToken: token, Description: desc,
		}
		if err := deps.Bots.Insert(r.Context(), bot); err != nil {
			uiBotListWithFlash(w, r, deps, tenant, "error", err.Error())
			return
		}
		startBotListener(deps, bot)
		uiBotListWithFlash(w, r, deps, tenant, "ok",
			"Bot 已创建。请到 Telegram 给该 bot 发送 /start，或将 bot 拉入群组，"+
				"bot 会自动回复对话的 Chat ID。把 Chat ID 填入新建 Channel 的表单。")
	}
}

// uiBotUpdate handles in-place edits from the shared edit-drawer. The
// drawer carries name / description / platform always, and bot_token
// only when the operator intentionally rotates the credential (blank
// input = keep current token). This lets routine renames stay cheap:
// only token + platform changes trigger a listener restart so the
// running long-poll loop is not bounced for cosmetic edits.
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
		platform := strings.TrimSpace(r.PostForm.Get("platform"))
		if platform == "" {
			platform = domain.PlatformTelegram
		}
		newToken := strings.TrimSpace(r.PostForm.Get("bot_token"))
		tenant := wmw.Tenant(r.Context())
		if name == "" {
			uiBotListWithFlash(w, r, deps, tenant, "error", "name 不能为空")
			return
		}
		if !domain.IsValidPlatform(platform) {
			uiBotListWithFlash(w, r, deps, tenant, "error", "未知 platform")
			return
		}
		// Validate the token format only when the operator actually
		// typed something. Blank means "keep existing".
		if newToken != "" && !botTokenPattern.MatchString(newToken) {
			uiBotListWithFlash(w, r, deps, tenant, "error", "bot_token 格式不正确")
			return
		}
		bot, err := deps.Bots.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			uiBotListWithFlash(w, r, deps, tenant, "error", "bot 不存在或不属于当前租户")
			return
		}
		tokenChanged := false
		platformChanged := bot.Platform != platform
		if newToken != "" && newToken != bot.BotToken {
			bot.BotToken = newToken
			tokenChanged = true
		}
		bot.Name = name
		bot.Description = desc
		bot.Platform = platform
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
		if tokenChanged || platformChanged {
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
