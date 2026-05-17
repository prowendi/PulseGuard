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
})();
