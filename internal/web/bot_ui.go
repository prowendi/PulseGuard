package web

import (
	"net/http"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
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
		views := make([]botView, 0, len(items))
		for _, b := range items {
			views = append(views, toBotView(b))
		}
		_ = Render(w, http.StatusOK, "bots-page", botListPage{
			pageData: pageData{
				Title:  "Bots",
				Tenant: tenant,
				Active: "bots",
				CSRF:   readCSRFCookie(r),
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
	views := make([]botView, 0, len(items))
	for _, b := range items {
		views = append(views, toBotView(b))
	}
	_ = Render(w, http.StatusOK, "bots-page", botListPage{
		pageData: pageData{
			Title:  "Bots",
			Tenant: tenant,
			Active: "bots",
			CSRF:   readCSRFCookie(r),
			Flash:  &flash{Level: level, Message: msg},
		},
		Bots: views,
	})
}
