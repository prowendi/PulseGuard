// PulseGuard glue layer: CSRF header injection for HTMX, toast helper,
// HTMX response-driven toast trigger, dropdown/user-menu toggles, and a
// tiny IIFE wrapper so nothing leaks into globals other than psgToast.
//
// CSP-strict: this file MUST be the only place that wires DOM events.
// Inline onclick/onsubmit handlers are forbidden so that script-src can
// stay at 'self' (no 'unsafe-inline'). See secureheaders.go.
(function () {
  function readCookie(name) {
    var pairs = (document.cookie || "").split(";");
    for (var i = 0; i < pairs.length; i++) {
      var idx = pairs[i].indexOf("=");
      if (idx < 0) continue;
      var k = pairs[i].slice(0, idx).trim();
      var v = pairs[i].slice(idx + 1).trim();
      if (k === name) return decodeURIComponent(v);
    }
    return "";
  }

  // deleteCookie expires the named cookie at "/" so a Set-Cookie max-age
  // flash can be consumed exactly once per page load. Same Path / SameSite
  // as the server-side issuer or browsers may not match.
  function deleteCookie(name) {
    document.cookie = name + "=; Path=/; Max-Age=0; SameSite=Lax";
  }

  // Ensure the toast stack <div> exists exactly once. Idempotent so we
  // can call it from psgToast even if the DOM was mutated by a swap.
  function ensureToastStack() {
    var stack = document.getElementById("psg-toast-stack");
    if (stack) return stack;
    stack = document.createElement("div");
    stack.id = "psg-toast-stack";
    document.body.appendChild(stack);
    return stack;
  }

  // psgToast(level, msg) — level ∈ {success,error,info}. 3s auto dismiss.
  window.psgToast = function (level, msg) {
    if (!msg) return;
    var lv = (level || "info").toLowerCase();
    if (lv !== "success" && lv !== "error") lv = "info";
    var stack = ensureToastStack();
    var t = document.createElement("div");
    t.className = "psg-toast psg-toast-" + lv;
    t.setAttribute("role", "status");
    t.innerHTML = '<span class="psg-toast-dot"></span><span>' +
      String(msg).replace(/[&<>"']/g, function (c) {
        return { "&": "&amp;", "<": "&lt;", ">": "&gt;", "\"": "&quot;", "'": "&#39;" }[c];
      }) + '</span>';
    stack.appendChild(t);
    setTimeout(function () {
      t.classList.add("psg-toast-out");
      setTimeout(function () { t.remove(); }, 220);
    }, 3000);
  };

  // positionPopoverFixed lifts a row-level dropdown to position:fixed
  // AND moves it under <body> so it escapes every ancestor's
  // overflow-hidden / overflow-x-auto clipping. The original DOM slot
  // is remembered on the element so clearPopoverFixed can put it back
  // exactly where it was — important because click-outside detection
  // calls pop.contains(target), and we still want the popover to act
  // logically scoped to the row that opened it.
  function positionPopoverFixed(btn, pop) {
    if (!pop.__psgDetached) {
      pop.__psgParent = pop.parentNode;
      pop.__psgNext = pop.nextSibling;
      document.body.appendChild(pop);
      pop.__psgDetached = true;
    }
    var rect = btn.getBoundingClientRect();
    var width = pop.offsetWidth || 176;
    // Clamp horizontally so the popover never spills past viewport.
    var right = window.innerWidth - rect.right;
    if (right < 8) right = 8;
    var maxRight = window.innerWidth - width - 8;
    if (right > maxRight) right = maxRight;
    pop.style.position = "fixed";
    pop.style.top = (rect.bottom + 4) + "px";
    pop.style.right = right + "px";
    pop.style.left = "";
    pop.style.zIndex = "40";
  }

  function clearPopoverFixed(pop) {
    pop.style.position = "";
    pop.style.top = "";
    pop.style.right = "";
    pop.style.left = "";
    pop.style.zIndex = "";
    if (pop.__psgDetached && pop.__psgParent) {
      try {
        if (pop.__psgNext && pop.__psgNext.parentNode === pop.__psgParent) {
          pop.__psgParent.insertBefore(pop, pop.__psgNext);
        } else {
          pop.__psgParent.appendChild(pop);
        }
      } catch (e) {
        // Original slot is gone (e.g., row was re-rendered); leave the
        // popover in body — closing still works via the hidden class.
      }
      pop.__psgDetached = false;
    }
  }

  // Toggle helper for any [data-psg-toggle="<targetId>"] button. Used by
  // the topbar user menu and the row-level "..." dropdowns on list pages.
  function bindToggles() {
    document.querySelectorAll("[data-psg-toggle]").forEach(function (btn) {
      if (btn.__psgBound) return;
      btn.__psgBound = true;
      btn.addEventListener("click", function (e) {
        e.preventDefault(); e.stopPropagation();
        var target = document.getElementById(btn.getAttribute("data-psg-toggle"));
        if (!target) return;
        // If we are about to OPEN a popover-style dropdown, switch it to
        // position:fixed so the parent card's overflow-hidden does not
        // clip it. Re-anchor on every open because the row may have
        // scrolled since the last time.
        var willOpen = target.classList.contains("hidden");
        if (willOpen && target.hasAttribute("data-psg-popover")) {
          positionPopoverFixed(btn, target);
        }
        var open = target.classList.toggle("hidden");
        btn.setAttribute("aria-expanded", String(!open));
        if (open && target.hasAttribute("data-psg-popover")) {
          clearPopoverFixed(target);
        }
      });
    });
    // Click-outside closes all dropdowns marked with data-psg-popover.
    document.addEventListener("click", function (e) {
      document.querySelectorAll("[data-psg-popover]").forEach(function (pop) {
        if (pop.classList.contains("hidden")) return;
        if (pop.contains(e.target)) return;
        pop.classList.add("hidden");
        clearPopoverFixed(pop);
      });
    }, { capture: true });
    // Scroll / resize invalidates the fixed coordinates we set on open;
    // close every visible popover so the next click re-anchors fresh.
    var dismissAll = function () {
      document.querySelectorAll("[data-psg-popover]").forEach(function (pop) {
        if (pop.classList.contains("hidden")) return;
        pop.classList.add("hidden");
        clearPopoverFixed(pop);
      });
    };
    window.addEventListener("scroll", dismissAll, { passive: true, capture: true });
    window.addEventListener("resize", dismissAll, { passive: true });
  }

  // applyBotDrawerVisibility toggles the bot drawer's conditional
  // field groups based on the current platform + bot_kind selection.
  //
  // Visibility matrix:
  //   - platform=telegram         → kind-row hidden, webhook-fields shown
  //   - platform=lark + webhook   → kind-row shown,  webhook-fields shown
  //   - platform=lark + app       → kind-row shown,  app-fields shown
  //
  // scope is "new" or "edit"; the data-scope attribute on each field
  // group lets the same helper drive both drawers.
  function applyBotDrawerVisibility(drawer, scope) {
    if (!drawer) return;
    var platSel = drawer.querySelector('[name="platform"]');
    var plat = platSel ? platSel.value : "telegram";
    var kindRow = drawer.querySelector('[data-scope="' + scope + '-kind-row"]');
    var webFields = drawer.querySelector('[data-scope="' + scope + '-webhook-fields"]');
    var appFields = drawer.querySelector('[data-scope="' + scope + '-app-fields"]');
    var showKindRow = (plat === "lark");
    var kind = "webhook";
    if (showKindRow) {
      var checked = drawer.querySelector('input[name="bot_kind"]:checked');
      if (checked) kind = checked.value;
    }
    if (kindRow) kindRow.classList.toggle("hidden", !showKindRow);
    if (webFields) webFields.classList.toggle("hidden", showKindRow && kind === "app");
    if (appFields) appFields.classList.toggle("hidden", !(showKindRow && kind === "app"));
  }

  // ---- Global data-action delegation ----------------------------------
  // One document-level click listener handles every declarative action,
  // so templates can use plain data-* attributes instead of inline
  // onclick handlers. Supported actions:
  //   data-action="drawer-open"    + data-target="drawer-x"
  //   data-action="drawer-close"   + data-target="drawer-x"
  //   data-action="copy-to-editor" + data-target="#command-code"
  //                                + data-code="..." (raw source to inject)
  // Confirm-on-submit is handled via data-confirm on the form OR on the
  // submit-triggering button (we register a separate submit listener).
  function bindActionDelegation() {
    document.addEventListener("click", function (e) {
      var node = e.target.closest ? e.target.closest("[data-action]") : null;
      if (!node) return;
      var action = node.getAttribute("data-action");
      if (!action) return;
      // Any edit-* action opens a drawer; close every row-level popover
      // first so the dropdown menu does not float on top of the editor
      // (and so we don't leak fixed-positioned children into <body>).
      if (action.indexOf("edit-") === 0) {
        document.querySelectorAll("[data-psg-popover]").forEach(function (pop) {
          if (pop.classList.contains("hidden")) return;
          pop.classList.add("hidden");
          clearPopoverFixed(pop);
        });
      }
      switch (action) {
        case "drawer-open": {
          e.preventDefault();
          var id = node.getAttribute("data-target");
          if (id && typeof window.psgOpenDrawer === "function") {
            window.psgOpenDrawer(id);
          }
          break;
        }
        case "drawer-close": {
          e.preventDefault();
          var idc = node.getAttribute("data-target");
          if (idc && typeof window.psgCloseDrawer === "function") {
            window.psgCloseDrawer(idc);
          }
          break;
        }
        case "copy-to-editor": {
          e.preventDefault();
          var sel = node.getAttribute("data-target") || "#command-code";
          var code = node.getAttribute("data-code") || "";
          var ta = document.querySelector(sel);
          if (!ta) return;
          ta.value = code;
          var drawer = node.getAttribute("data-drawer");
          if (drawer && typeof window.psgOpenDrawer === "function") {
            window.psgOpenDrawer(drawer);
          }
          try { ta.focus({ preventScroll: true }); } catch (err) { ta.focus(); }
          break;
        }
        case "theme-cycle": {
          // Three-state toggle: auto ("") → light → dark → auto. Persisted
          // via the psg-theme cookie (SameSite=Lax, 1y) so the next first
          // paint reads the cookie server-side and emits <html class="dark">
          // directly — no flicker, no inline <script>. We reload after
          // setting the cookie because Tailwind's compiled CSS targets
          // .dark via selector overrides shipped in app.css; swapping the
          // <html> class at runtime would otherwise leave hover/focus
          // variants stale until the next navigation.
          e.preventDefault();
          var cur = readCookie("psg-theme");
          var next = "";
          if (!cur) next = "light";
          else if (cur === "light") next = "dark";
          else next = ""; // dark → auto (clear cookie)
          if (next === "") {
            // Expire the cookie at "/" so themeFromRequest() reads auto.
            document.cookie = "psg-theme=; Path=/; Max-Age=0; SameSite=Lax";
          } else {
            // 31_536_000s = 1 year. Path=/ keeps it readable across the
            // whole UI surface. SameSite=Lax is the same posture as the
            // session cookie so cross-site GETs do not lose preference.
            document.cookie = "psg-theme=" + next + "; Path=/; Max-Age=31536000; SameSite=Lax";
          }
          window.location.reload();
          break;
        }
        case "edit-template": {
          // Shared edit-drawer for the templates page. The trigger row
          // carries data-id/-name/-parse-mode/-body; we hydrate the
          // form fields, retarget the form action to the per-row update
          // URL, then open the drawer. Body roundtrips losslessly via
          // dataset because html/template attribute-escapes \n and ".
          e.preventDefault();
          var drawer = document.getElementById("drawer-edit-tpl");
          if (!drawer) return;
          var form = drawer.querySelector("form");
          if (form) {
            form.action = "/ui/templates/" + node.getAttribute("data-id") + "/update";
          }
          var setVal = function (sel, v) {
            var el = drawer.querySelector(sel);
            if (el) el.value = v || "";
          };
          setVal('[name="name"]', node.getAttribute("data-name"));
          setVal('[name="parse_mode"]', node.getAttribute("data-parse-mode"));
          setVal('[name="body"]', node.getAttribute("data-body"));
          if (typeof window.psgOpenDrawer === "function") {
            window.psgOpenDrawer("drawer-edit-tpl");
          }
          break;
        }
        case "edit-channel": {
          // Hydrate the shared channel edit drawer: scalar fields by
          // name, plus a JSON-encoded bindings array (template_id +
          // is_default + condition) that we walk to pre-check the
          // right checkboxes and fill per-template condition inputs.
          e.preventDefault();
          var ced = document.getElementById("drawer-edit-ch");
          if (!ced) return;
          var cef = ced.querySelector("form");
          if (cef) {
            cef.action = "/ui/channels/" + node.getAttribute("data-id") + "/update";
          }
          var setChVal = function (sel, v) {
            var el = ced.querySelector(sel);
            if (el) el.value = v == null ? "" : v;
          };
          setChVal('[name="name"]', node.getAttribute("data-name"));
          setChVal('[name="chat_id"]', node.getAttribute("data-chat-id"));
          setChVal('[name="bot_id"]', node.getAttribute("data-bot-id"));
          setChVal('[name="rate_per_min"]', node.getAttribute("data-rate-per-min"));
          setChVal('[name="dedup_window_s"]', node.getAttribute("data-dedup-window-s"));
          var enabledCb = ced.querySelector('[name="enabled"]');
          if (enabledCb) {
            enabledCb.checked = node.getAttribute("data-enabled") === "1";
          }
          // Reset every binding row first so a previously opened edit
          // doesn't leak state into the current one.
          ced.querySelectorAll('[data-edit-tpl-id]').forEach(function (row) {
            var cb = row.querySelector('input[type="checkbox"]');
            var cond = row.querySelector('input[name="conditions"]');
            if (cb) cb.checked = false;
            if (cond) cond.value = "";
          });
          var bindingsRaw = node.getAttribute("data-bindings") || "[]";
          try {
            var bindings = JSON.parse(bindingsRaw);
            if (Array.isArray(bindings)) {
              bindings.forEach(function (b) {
                var row = ced.querySelector('[data-edit-tpl-id="' + b.template_id + '"]');
                if (!row) return;
                var cb = row.querySelector('input[type="checkbox"]');
                var cond = row.querySelector('input[name="conditions"]');
                if (cb) cb.checked = true;
                if (cond) cond.value = b.condition || "";
              });
            }
          } catch (parseErr) {
            // swallow — leaving rows unchecked is the safe fallback
          }
          if (typeof window.psgOpenDrawer === "function") {
            window.psgOpenDrawer("drawer-edit-ch");
          }
          break;
        }
        case "edit-command": {
          // Hydrate the shared command edit drawer: name / description
          // / code / enabled. Code roundtrips losslessly because
          // html/template attribute-escapes newlines + quotes; the
          // dataset reader recovers the original source verbatim.
          e.preventDefault();
          var dec = document.getElementById("drawer-edit-cmd");
          if (!dec) return;
          var def = dec.querySelector("form");
          if (def) {
            def.action = "/ui/commands/" + node.getAttribute("data-id") + "/update";
          }
          var setCmdVal = function (sel, v) {
            var el = dec.querySelector(sel);
            if (el) el.value = v == null ? "" : v;
          };
          setCmdVal('[name="name"]', node.getAttribute("data-name"));
          setCmdVal('[name="description"]', node.getAttribute("data-description"));
          setCmdVal('[name="code"]', node.getAttribute("data-code"));
          var ecb = dec.querySelector('[name="enabled"]');
          if (ecb) {
            ecb.checked = node.getAttribute("data-enabled") === "1";
          }
          if (typeof window.psgOpenDrawer === "function") {
            window.psgOpenDrawer("drawer-edit-cmd");
          }
          break;
        }
        case "edit-bot": {
          // Shared edit-drawer for the bots page. Row carries data-id /
          // -name / -description / -platform / -bot-kind / -app-id /
          // -verify-token / -encrypt-key. bot_token and app_secret are
          // intentionally NOT round-tripped — those fields are left
          // blank and the server reads "blank = keep current secret".
          // Only when the operator types a new value does the listener
          // restart or the secret rotate.
          e.preventDefault();
          var dbot = document.getElementById("drawer-edit-bot");
          if (!dbot) return;
          var fbot = dbot.querySelector("form");
          if (fbot) {
            fbot.action = "/ui/bots/" + node.getAttribute("data-id") + "/update";
          }
          var setBot = function (sel, v) {
            var el = dbot.querySelector(sel);
            if (el) el.value = v || "";
          };
          setBot('[name="name"]', node.getAttribute("data-name"));
          setBot('[name="description"]', node.getAttribute("data-description"));
          setBot('[name="platform"]', node.getAttribute("data-platform"));
          setBot('[name="bot_token"]', ""); // explicit blank = keep token
          setBot('[name="app_id"]', node.getAttribute("data-app-id"));
          setBot('[name="app_secret"]', ""); // explicit blank = keep secret
          setBot('[name="verify_token"]', node.getAttribute("data-verify-token"));
          setBot('[name="encrypt_key"]', node.getAttribute("data-encrypt-key"));
          // Set bot_kind radio (default webhook for legacy rows). Find
          // the matching radio button in the edit drawer and tick it.
          var kind = node.getAttribute("data-bot-kind") || "webhook";
          var radios = dbot.querySelectorAll('[name="bot_kind"]');
          radios.forEach(function (r) {
            r.checked = (r.value === kind);
          });
          // Drive the conditional sections via applyBotDrawerVisibility
          // so the freshly-prefilled drawer matches the row's actual
          // platform+kind state on first paint.
          applyBotDrawerVisibility(dbot, "edit");
          if (typeof window.psgOpenDrawer === "function") {
            window.psgOpenDrawer("drawer-edit-bot");
          }
          break;
        }
        case "lark-kind-changed": {
          // Bot drawer (new or edit): the operator toggled the
          // webhook/app radio. Re-apply visibility so the right field
          // group is shown.
          var scope = node.getAttribute("data-scope") || "new";
          var drawer = document.getElementById(scope === "edit" ? "drawer-edit-bot" : "drawer-new-bot");
          if (drawer) applyBotDrawerVisibility(drawer, scope);
          break;
        }
        case "lark-kind-platform": {
          // Bot drawer (new or edit): the operator changed the platform
          // <select>. Show / hide the lark-kind radio row + the
          // appropriate field group.
          var pscope = node.getAttribute("data-scope") || "new";
          var pdrawer = document.getElementById(pscope === "edit" ? "drawer-edit-bot" : "drawer-new-bot");
          if (pdrawer) applyBotDrawerVisibility(pdrawer, pscope);
          break;
        }
        case "tpl-preview": {
          // V6-5 template live-preview: POST the current Body +
          // parse_mode + sample payload to /api/v1/templates/preview
          // and dump the rendered text into the panel. The drawer
          // scope ("new"|"edit") picks which textareas/output to read.
          // CSRF token is pulled from the psg_csrf cookie so we obey
          // the same protections as the rest of the authed API surface.
          e.preventDefault();
          var scope = node.getAttribute("data-scope") || "new";
          var panel = document.querySelector('[data-tpl-preview-scope="' + scope + '"]');
          if (!panel) return;
          var drawerId = scope === "edit" ? "drawer-edit-tpl" : "drawer-new-tpl";
          var drawer = document.getElementById(drawerId);
          if (!drawer) return;
          var bodyEl = drawer.querySelector('[name="body"]');
          var modeEl = drawer.querySelector('[name="parse_mode"]');
          var sampleEl = panel.querySelector('[data-tpl-sample]');
          var outEl = panel.querySelector('[data-tpl-preview-out]');
          if (!bodyEl || !modeEl || !outEl) return;
          var sample = {};
          if (sampleEl && sampleEl.value.trim() !== "") {
            try {
              sample = JSON.parse(sampleEl.value);
            } catch (parseErr) {
              outEl.textContent = "示例 payload 不是合法 JSON: " + parseErr.message;
              return;
            }
          }
          outEl.textContent = "渲染中...";
          var token = readCookie("psg_csrf");
          fetch("/api/v1/templates/preview", {
            method: "POST",
            credentials: "same-origin",
            headers: {
              "Content-Type": "application/json",
              "X-CSRF-Token": token || ""
            },
            body: JSON.stringify({
              body: bodyEl.value,
              parse_mode: modeEl.value,
              sample: sample
            })
          }).then(function (resp) {
            return resp.json().then(function (data) {
              if (!resp.ok) {
                var msg = (data && (data.error || data.message)) || ("HTTP " + resp.status);
                outEl.textContent = "预览失败: " + msg;
                return;
              }
              outEl.textContent = (data && (data.rendered || "")) || "(空)";
            });
          }).catch(function (err) {
            outEl.textContent = "预览失败: " + (err && err.message ? err.message : err);
          });
          break;
        }
        default:
          break;
      }
    });
  }

  // Form-level confirm. The contract is: any <form> with data-confirm="…"
  // shows that confirmation prompt at submit time; the user can cancel by
  // clicking "Cancel" which aborts the submission. Mirrors the legacy
  // onsubmit="return confirm(...)" pattern without inline script.
  function bindConfirmSubmit() {
    document.addEventListener("submit", function (e) {
      var form = e.target;
      if (!form || form.nodeName !== "FORM") return;
      var msg = form.getAttribute("data-confirm");
      if (!msg) return;
      if (!window.confirm(msg)) {
        e.preventDefault();
        e.stopImmediatePropagation();
      }
    }, { capture: true });
  }

  // Flash cookie consumer. Server-side handlers may set a short-lived
  // psg_flash cookie shaped "<level>:<message>" after a redirect-after-
  // POST so we can pop a toast on the next GET without re-rendering the
  // page-level flash partial. Consumed exactly once.
  function consumeFlashCookie() {
    var raw = readCookie("psg_flash");
    if (!raw) return;
    deleteCookie("psg_flash");
    var ix = raw.indexOf(":");
    var level = "info";
    var msg = raw;
    if (ix >= 0) {
      level = raw.slice(0, ix).trim() || "info";
      msg = raw.slice(ix + 1).trim();
    }
    if (msg) window.psgToast(level, msg);
  }

  // HTMX → CSRF header.
  document.addEventListener("DOMContentLoaded", function () {
    document.body.addEventListener("htmx:configRequest", function (evt) {
      var token = readCookie("psg_csrf");
      if (token) evt.detail.headers["X-CSRF-Token"] = token;
    });
    // Servers can emit `X-Psg-Toast: success:消息` (or error:/info:) and
    // the toast appears automatically — no JSON parsing needed.
    document.body.addEventListener("htmx:afterRequest", function (evt) {
      var hdr = evt.detail.xhr && evt.detail.xhr.getResponseHeader("X-Psg-Toast");
      if (!hdr) return;
      var ix = hdr.indexOf(":");
      if (ix < 0) { window.psgToast("info", hdr); return; }
      window.psgToast(hdr.slice(0, ix).trim(), hdr.slice(ix + 1).trim());
    });
    bindToggles();
    bindActionDelegation();
    bindBotDrawerChangeEvents();
    bindConfirmSubmit();
    ensureToastStack();
    consumeFlashCookie();
  });
  // HTMX swaps may inject new toggle buttons; re-bind after each swap.
  document.addEventListener("htmx:afterSwap", bindToggles);

  // bindBotDrawerChangeEvents is a tiny "change"-event delegate that
  // catches platform <select> and bot_kind radio changes inside any bot
  // drawer (new + edit) and routes them through applyBotDrawerVisibility.
  // The data-action="lark-kind-changed" / "lark-kind-platform" click
  // handlers already cover the radio-toggle case for browsers that
  // surface click on the label; the change listener is the belt-and-
  // braces path for the <select> (which never fires click on value
  // change) and for keyboard-only users.
  function bindBotDrawerChangeEvents() {
    document.addEventListener("change", function (e) {
      var t = e.target;
      if (!t || !t.getAttribute) return;
      var action = t.getAttribute("data-action");
      if (action !== "lark-kind-changed" && action !== "lark-kind-platform") return;
      var scope = t.getAttribute("data-scope") || "new";
      var drawer = document.getElementById(scope === "edit" ? "drawer-edit-bot" : "drawer-new-bot");
      if (drawer) applyBotDrawerVisibility(drawer, scope);
    });
  }

  // ---- Right-side drawer (Linear/Vercel style) -------------------------
  // Markup contract (data-action wired from app.js — no inline handlers):
  //   <div id="drawer-xxx" class="psg-drawer fixed inset-0 z-40 hidden"
  //        role="dialog" aria-modal="true" aria-labelledby="drawer-xxx-title">
  //     <div class="psg-drawer-backdrop ..."
  //          data-action="drawer-close" data-target="drawer-xxx"></div>
  //     <div class="psg-drawer-panel ... translate-x-full">...</div>
  //   </div>
  // The 300ms slide is owned by Tailwind transition utilities; we toggle
  // translate-x-full / opacity-0 classes here and let CSS animate.
  window.psgOpenDrawer = function (id) {
    var el = document.getElementById(id);
    if (!el) return;
    el.classList.remove("hidden");
    // Force reflow so the subsequent class change triggers a transition
    // rather than a synchronous paint (otherwise the slide is invisible
    // because the element was hidden when the panel had translate-x-full).
    void el.offsetHeight;
    var backdrop = el.querySelector(".psg-drawer-backdrop");
    var panel = el.querySelector(".psg-drawer-panel");
    if (backdrop) {
      backdrop.classList.remove("opacity-0");
      backdrop.classList.add("opacity-100");
    }
    if (panel) {
      panel.classList.remove("translate-x-full");
    }
    document.body.style.overflow = "hidden";
    // Focus the first interactive field so keyboard users land in-form.
    var f = el.querySelector("input,textarea,select,button");
    if (f) {
      try { f.focus({ preventScroll: true }); } catch (e) { f.focus(); }
    }
  };

  window.psgCloseDrawer = function (id) {
    var el = document.getElementById(id);
    if (!el) return;
    var backdrop = el.querySelector(".psg-drawer-backdrop");
    var panel = el.querySelector(".psg-drawer-panel");
    if (panel) panel.classList.add("translate-x-full");
    if (backdrop) {
      backdrop.classList.remove("opacity-100");
      backdrop.classList.add("opacity-0");
    }
    setTimeout(function () {
      el.classList.add("hidden");
      // Only release body scroll lock when no other drawer is open.
      var stillOpen = document.querySelector(".psg-drawer:not(.hidden)");
      if (!stillOpen) document.body.style.overflow = "";
    }, 300);
  };

  // ESC closes any currently open drawer.
  document.addEventListener("keydown", function (e) {
    if (e.key !== "Escape") return;
    document.querySelectorAll(".psg-drawer:not(.hidden)").forEach(function (d) {
      window.psgCloseDrawer(d.id);
    });
  });

  // Deep-link via hash (e.g. /ui/bots#drawer-new-bot opens the drawer).
  // Runs after DOMContentLoaded so the drawer node is guaranteed parsed.
  window.addEventListener("DOMContentLoaded", function () {
    if (!location.hash) return;
    var id = location.hash.slice(1);
    var el = document.getElementById(id);
    if (el && el.classList.contains("psg-drawer")) {
      window.psgOpenDrawer(id);
    }
  });
})();
