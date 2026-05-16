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
		if !VerifyCSRF(r, deps.csrfSecret()) {
			http.Error(w, "csrf", http.StatusForbidden)
			return
		}
		_ = r.ParseForm()
		tenant := wmw.Tenant(r.Context())
		name := strings.TrimSpace(r.PostForm.Get("name"))
		chatID := strings.TrimSpace(r.PostForm.Get("chat_id"))
		botID, _ := strconv.ParseInt(r.PostForm.Get("bot_id"), 10, 64)
		rate, _ := strconv.Atoi(r.PostForm.Get("rate_per_min"))
		dedup, _ := strconv.Atoi(r.PostForm.Get("dedup_window_s"))

		// Multi-select templates: form sends every checked id under
		// the same key ("template_ids"); default is a single id under
		// "default_template_id". If only one template_id arrived
		// (legacy single-select form fall-back), accept it too.
		var templateIDs []int64
		for _, v := range r.PostForm["template_ids"] {
			if id, err := strconv.ParseInt(v, 10, 64); err == nil && id > 0 {
				templateIDs = append(templateIDs, id)
			}
		}
		// Back-compat: legacy single dropdown (B3 will remove this).
		if len(templateIDs) == 0 {
			if id, err := strconv.ParseInt(r.PostForm.Get("template_id"), 10, 64); err == nil && id > 0 {
				templateIDs = append(templateIDs, id)
			}
		}
		defID, _ := strconv.ParseInt(r.PostForm.Get("default_template_id"), 10, 64)

		if name == "" || botID == 0 || chatID == "" || len(templateIDs) == 0 {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: "请填写完整 name/bot/chat_id 并选择至少一个模板"})
			return
		}
		// Validate ownership.
		if _, err := deps.Bots.GetByID(r.Context(), tenant.ID, botID); err != nil {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: "bot 不存在或不属于当前租户"})
			return
		}
		bindings, err := buildUIBindings(r.Context(), deps, tenant.ID, templateIDs, defID)
		if err != nil {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: err.Error()})
			return
		}
		ch := &domain.Channel{
			TenantID: tenant.ID, Name: name,
			PushToken:    newPushToken(),
			BotID:        botID,
			ChatID:       chatID,
			RatePerMin:   rate,
			DedupWindowS: dedup,
			Enabled:      true,
			Templates:    bindings,
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
		if !VerifyCSRF(r, deps.csrfSecret()) {
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
		if !VerifyCSRF(r, deps.csrfSecret()) {
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
		ch.Templates = nil
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
