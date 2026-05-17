// PulseGuard glue layer: CSRF header injection for HTMX, toast helper,
// HTMX response-driven toast trigger, dropdown/user-menu toggles, and a
// tiny IIFE wrapper so nothing leaks into globals other than psgToast.
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
    ensureToastStack();
  });
  // HTMX swaps may inject new toggle buttons; re-bind after each swap.
  document.addEventListener("htmx:afterSwap", bindToggles);

  // ---- Right-side drawer (Linear/Vercel style) -------------------------
  // Markup contract (inlined per page to avoid template.HTML XSS risk):
  //   <div id="drawer-xxx" class="psg-drawer fixed inset-0 z-40 hidden"
  //        role="dialog" aria-modal="true" aria-labelledby="drawer-xxx-title">
  //     <div class="psg-drawer-backdrop ..." onclick="psgCloseDrawer('drawer-xxx')"></div>
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
