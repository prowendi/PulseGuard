package web

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/wendi/pulseguard/internal/domain"
	wmw "github.com/wendi/pulseguard/internal/web/middleware"

	"github.com/go-chi/chi/v5"
)

// channelListPage feeds /ui/channels with all the dropdown source data.
type channelListPage struct {
	pageData
	Channels  []channelView
	Bots      []botView
	Templates []templateView
}

func installChannelsUIRoutes(r chi.Router, deps Deps) {
	r.Get("/channels", uiChannelList(deps))
	r.Post("/channels", uiChannelCreate(deps))
	r.Post("/channels/{id}/delete", uiChannelDelete(deps))
	r.Post("/channels/{id}/rotate-token", uiChannelRotate(deps))
}

func uiChannelList(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tenant := wmw.Tenant(r.Context())
		renderChannelPage(w, r, deps, tenant, nil)
	}
}

func uiChannelCreate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		tenant := wmw.Tenant(r.Context())
		name := strings.TrimSpace(r.PostForm.Get("name"))
		chatID := strings.TrimSpace(r.PostForm.Get("chat_id"))
		botID, _ := strconv.ParseInt(r.PostForm.Get("bot_id"), 10, 64)
		tplID, _ := strconv.ParseInt(r.PostForm.Get("template_id"), 10, 64)
		rate, _ := strconv.Atoi(r.PostForm.Get("rate_per_min"))
		dedup, _ := strconv.Atoi(r.PostForm.Get("dedup_window_s"))
		if name == "" || botID == 0 || tplID == 0 || chatID == "" {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: "请填写完整 name/bot/template/chat_id"})
			return
		}
		ch := &domain.Channel{
			TenantID: tenant.ID, Name: name,
			PushToken: newPushToken(),
			BotID:     botID, TemplateID: tplID,
			ChatID:       chatID,
			RatePerMin:   rate,
			DedupWindowS: dedup,
			Enabled:      true,
		}
		if err := deps.Channels.Insert(r.Context(), ch); err != nil {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: err.Error()})
			return
		}
		http.Redirect(w, r, "/ui/channels", http.StatusSeeOther)
	}
}

func uiChannelDelete(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		_ = deps.Channels.Delete(r.Context(), tenant.ID, id)
		http.Redirect(w, r, "/ui/channels", http.StatusSeeOther)
	}
}

func uiChannelRotate(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !VerifyCSRF(r) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		id, ok := parsePathID(w, r, "id")
		if !ok {
			return
		}
		tenant := wmw.Tenant(r.Context())
		ch, err := deps.Channels.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: err.Error()})
			return
		}
		ch.PushToken = newPushToken()
		_ = deps.Channels.Update(r.Context(), ch)
		http.Redirect(w, r, "/ui/channels", http.StatusSeeOther)
	}
}

func renderChannelPage(w http.ResponseWriter, r *http.Request, deps Deps, tenant *domain.Tenant, fl *flash) {
	chList, _ := deps.Channels.ListByTenant(r.Context(), tenant.ID)
	botsList, _ := deps.Bots.ListByTenant(r.Context(), tenant.ID)
	tplList, _ := deps.Templates.ListByTenant(r.Context(), tenant.ID)
	channels := make([]channelView, 0, len(chList))
	for _, c := range chList {
		channels = append(channels, toChannelView(c))
	}
	bots := make([]botView, 0, len(botsList))
	for _, b := range botsList {
		bots = append(bots, toBotView(b))
	}
	tpls := make([]templateView, 0, len(tplList))
	for _, t := range tplList {
		tpls = append(tpls, toTemplateView(t))
	}
	_ = Render(w, http.StatusOK, "channels-page", channelListPage{
		pageData: pageData{
			Title:  "通道",
			Tenant: tenant,
			Active: "channels",
			CSRF:   readCSRFCookie(r),
			Flash:  fl,
		},
		Channels:  channels,
		Bots:      bots,
		Templates: tpls,
	})
}
