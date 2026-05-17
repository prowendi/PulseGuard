// PulseGuard template editor — toolbar insertion + Demo Gallery copy +
// MarkdownV2 fool-proof affordances.
//
// Wires up:
//   * data-tpl-insert buttons → insert snippet at the caret of whichever
//     template-body textarea is currently active (new-drawer or
//     edit-drawer). The "escape" variant wraps the current selection.
//   * data-tpl-demo buttons → pull the demo body / parse_mode / name
//     from the button's data-* attributes (the previous version walked
//     btn.closest("div") which silently picked up the wrong wrapper) and
//     populate the new-drawer form before opening it.
//   * Bold / italic / code wrapping with MarkdownV2-safe escaping.
//
// Per-drawer scoping rule: every textarea action targets the textarea
// inside the SAME drawer as the button. This eliminates the pre-fix
// bug where edit-drawer toolbar clicks wrote into the closed new-drawer
// textarea (because both shared `data-tpl-insert`).
(function () {
  function $(sel, root) { return (root || document).querySelector(sel); }
  function $all(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }

  // Snippet generators for the toolbar. Each returns the literal text
  // to insert at the caret, or null to abort (user cancelled a prompt).
  var snippets = {
    "var": function () {
      var name = window.prompt("插入字段（从 payload JSON 取值，自动 escMD）", "title");
      if (!name) return null;
      return "{{ ." + name.replace(/[^A-Za-z0-9_.]/g, "") + " | escMD }}";
    },
    "cond": function () {
      return "{{ if eq .level \"critical\" }}🚨{{ else }}⚠️{{ end }}";
    },
    "loop": function () {
      return "{{ range .items }}\n  - {{ . | escMD }}\n{{ end }}";
    },
    "link": function () {
      var label = window.prompt("链接文本", "查看详情");
      if (!label) return null;
      return "[" + label + "]({{ .url }})";
    },
    "code": function () {
      return "```\n{{ .code }}\n```";
    },
    // bold / italic wrap the selection (or insert a placeholder) using
    // MarkdownV2 syntax — note the asterisks/underscores must NOT be
    // escMD'd themselves; only the inner content needs escMD.
    "bold": null,
    "italic": null,
    "escape": null
  };

  function insertAtCaret(textarea, text) {
    var start = textarea.selectionStart || 0;
    var end = textarea.selectionEnd || 0;
    var v = textarea.value;
    textarea.value = v.slice(0, start) + text + v.slice(end);
    textarea.selectionStart = textarea.selectionEnd = start + text.length;
    textarea.focus();
  }

  function wrapSelection(textarea, before, after, placeholderHint) {
    var start = textarea.selectionStart || 0;
    var end = textarea.selectionEnd || 0;
    var v = textarea.value;
    var sel = v.slice(start, end);
    if (!sel) {
      // No selection: insert "{before}<placeholder>{after}" so the user
      // sees a working example without an alert popup.
      var placeholder = placeholderHint || "内容";
      var newText = before + placeholder + after;
      textarea.value = v.slice(0, start) + newText + v.slice(end);
      // Select the placeholder so the next keypress overwrites it.
      textarea.selectionStart = start + before.length;
      textarea.selectionEnd = start + before.length + placeholder.length;
    } else {
      var combined = before + sel + after;
      textarea.value = v.slice(0, start) + combined + v.slice(end);
      textarea.selectionStart = start;
      textarea.selectionEnd = start + combined.length;
    }
    textarea.focus();
  }

  // textareaForButton walks up from a toolbar button to the enclosing
  // drawer (data-action="drawer-close" backdrop carries the drawer id
  // via data-target) and returns the textarea inside that drawer.
  // Falls back to #tplBody if the button is not inside a drawer, which
  // covers the case where toolbars get rendered outside both drawers
  // for any reason.
  function textareaForButton(btn) {
    var drawer = btn.closest('.psg-drawer');
    if (drawer) {
      // Per-drawer textareas: prefer one with name="body" (matches the
      // form contract). Both new- and edit- drawers use the same name.
      return drawer.querySelector('textarea[name="body"]');
    }
    return $('#tplBody');
  }

  // bindToolbarOnce attaches a single delegated listener so dynamically
  // re-rendered drawers continue to work. The listener filters on the
  // data-tpl-insert attribute.
  function bindToolbar() {
    document.addEventListener('click', function (e) {
      var btn = e.target.closest ? e.target.closest('[data-tpl-insert]') : null;
      if (!btn) return;
      var ta = textareaForButton(btn);
      if (!ta) return;
      var kind = btn.getAttribute('data-tpl-insert');
      switch (kind) {
        case 'bold':
          wrapSelection(ta, "*", "*", "粗体文字");
          return;
        case 'italic':
          wrapSelection(ta, "_", "_", "斜体文字");
          return;
        case 'escape':
          wrapSelection(ta, "{{ ", " | escMD }}", ".field");
          return;
      }
      var fn = snippets[kind];
      if (!fn) return;
      var txt = fn();
      if (txt == null) return;
      insertAtCaret(ta, txt);
    });
  }

  // bindDemos handles "复制到编辑器" — reads body / parse_mode / name
  // from the button's data-* attributes (set by the template). This is
  // a delegated listener so demos can be re-rendered without re-binding.
  function bindDemos() {
    document.addEventListener('click', function (e) {
      var btn = e.target.closest ? e.target.closest('[data-tpl-demo]') : null;
      if (!btn) return;
      e.preventDefault();
      var body = btn.getAttribute('data-body') || '';
      var mode = btn.getAttribute('data-mode') || 'MarkdownV2';
      var name = btn.getAttribute('data-name') || '';
      var ta = $('#tplBody');
      if (!ta) return;
      if (ta.value.trim() && !window.confirm("当前编辑器有内容，是否覆盖？")) {
        return;
      }
      ta.value = body;
      // Auto-fill the new-template form so the operator sees a complete
      // pre-populated record. parse_mode + name pulled from the demo
      // metadata; the operator can still rename before saving.
      var form = ta.closest('form');
      if (form) {
        var nameInput = form.querySelector('input[name="name"]');
        if (nameInput && !nameInput.value.trim() && name) {
          // Suggest a slug-style name based on the demo id/title.
          nameInput.value = (name + "").toLowerCase().replace(/[^a-z0-9-]+/g, "-").replace(/^-|-$/g, "");
        }
        var modeSelect = form.querySelector('select[name="parse_mode"]');
        if (modeSelect && mode) {
          modeSelect.value = mode;
        }
      }
      // Auto-open the new-template drawer so the user sees the
      // populated editor immediately after copying a demo.
      if (typeof window.psgOpenDrawer === "function") {
        window.psgOpenDrawer("drawer-new-tpl");
      }
      ta.focus();
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    bindToolbar();
    bindDemos();
  });
})();
