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
	r.Post("/channels/{id}/update", uiChannelUpdate(deps))
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
		// (legacy single-select form fall-back), accept it too. The
		// optional "conditions" repeats the same number of values, one
		// per template_ids entry in order.
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
		conditions := r.PostForm["conditions"]
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
		bindings, err := buildUIBindings(r.Context(), deps, tenant.ID, templateIDs, defID, conditions)
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

// uiChannelUpdate handles in-place edits from the shared edit-drawer.
// The drawer always re-submits the full template binding set so the
// edit semantic stays "what you see is what gets persisted" — the
// per-binding condition + default-flag are likewise re-derived from
// the form, never inherited silently from the prior state. This
// keeps the handler symmetrical with uiChannelCreate; the only
// difference is GetByID + Update instead of Insert, and the
// ReplaceTemplates call to atomically swap the binding rows.
func uiChannelUpdate(deps Deps) http.HandlerFunc {
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
		tenant := wmw.Tenant(r.Context())
		name := strings.TrimSpace(r.PostForm.Get("name"))
		chatID := strings.TrimSpace(r.PostForm.Get("chat_id"))
		botID, _ := strconv.ParseInt(r.PostForm.Get("bot_id"), 10, 64)
		rate, _ := strconv.Atoi(r.PostForm.Get("rate_per_min"))
		dedup, _ := strconv.Atoi(r.PostForm.Get("dedup_window_s"))
		enabled := r.PostForm.Get("enabled") != ""

		var templateIDs []int64
		for _, v := range r.PostForm["template_ids"] {
			if tid, err := strconv.ParseInt(v, 10, 64); err == nil && tid > 0 {
				templateIDs = append(templateIDs, tid)
			}
		}
		conditions := r.PostForm["conditions"]
		defID, _ := strconv.ParseInt(r.PostForm.Get("default_template_id"), 10, 64)

		if name == "" || botID == 0 || chatID == "" || len(templateIDs) == 0 {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: "请填写完整 name/bot/chat_id 并选择至少一个模板"})
			return
		}
		if rate < 0 || dedup < 0 {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: "rate_per_min 与 dedup_window_s 不能为负"})
			return
		}
		if _, err := deps.Bots.GetByID(r.Context(), tenant.ID, botID); err != nil {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: "bot 不存在或不属于当前租户"})
			return
		}
		ch, err := deps.Channels.GetByID(r.Context(), tenant.ID, id)
		if err != nil {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: "通道不存在或不属于当前租户"})
			return
		}
		bindings, err := buildUIBindings(r.Context(), deps, tenant.ID, templateIDs, defID, conditions)
		if err != nil {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: err.Error()})
			return
		}
		ch.Name = name
		ch.BotID = botID
		ch.ChatID = chatID
		ch.RatePerMin = rate
		ch.DedupWindowS = dedup
		ch.Enabled = enabled
		// Update must NOT replace bindings — that's ReplaceTemplates' job
		// (atomic swap). Clearing the slice prevents a stale set from
		// the GetByID call from leaking into the scalar UPDATE path.
		ch.Templates = nil
		if err := deps.Channels.Update(r.Context(), ch); err != nil {
			renderChannelPage(w, r, deps, tenant, &flash{Level: "error", Message: err.Error()})
			return
		}
		if err := deps.Channels.ReplaceTemplates(r.Context(), tenant.ID, ch.ID, bindings); err != nil {
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
			Theme:  themeFromRequest(r),
		},
		Channels:  channels,
		Bots:      bots,
		Templates: tpls,
	})
}
