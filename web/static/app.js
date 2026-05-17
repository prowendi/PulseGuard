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
        var open = target.classList.toggle("hidden");
        btn.setAttribute("aria-expanded", String(!open));
      });
    });
    // Click-outside closes all dropdowns marked with data-psg-popover.
    document.addEventListener("click", function (e) {
      document.querySelectorAll("[data-psg-popover]").forEach(function (pop) {
        if (pop.classList.contains("hidden")) return;
        if (pop.contains(e.target)) return;
        pop.classList.add("hidden");
      });
    }, { capture: true });
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
        case "edit-bot": {
          // Shared edit-drawer for the bots page. Row carries data-id /
          // -name / -description / -platform. bot_token is intentionally
          // NOT round-tripped — the field is left blank and the server
          // reads "blank = keep current token". Only when the operator
          // types a new value does the listener restart.
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
          if (typeof window.psgOpenDrawer === "function") {
            window.psgOpenDrawer("drawer-edit-bot");
          }
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
    bindConfirmSubmit();
    ensureToastStack();
    consumeFlashCookie();
  });
  // HTMX swaps may inject new toggle buttons; re-bind after each swap.
  document.addEventListener("htmx:afterSwap", bindToggles);

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
