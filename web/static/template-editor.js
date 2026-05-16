// PulseGuard template editor — toolbar insertion + Demo Gallery copy.
//
// Wires up:
//   * data-tpl-insert buttons → insert snippet at the textarea caret
//     (or wrap the current selection for the "escape" variant).
//   * data-tpl-demo buttons → POST nothing; pull the preset body from the
//     adjacent <pre> and replace the textarea content (with confirmation
//     if non-empty so the user does not lose an in-progress draft).
(function () {
  function $(sel, root) { return (root || document).querySelector(sel); }
  function $all(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }

  // Snippet table for the toolbar. Strings are inserted raw; the {var}
  // placeholder is replaced via a prompt() so the user can name the
  // payload field.
  var snippets = {
    "var":    function () {
      var name = window.prompt("字段名 (从 payload 取)", "title");
      if (!name) return null;
      return "{{ ." + name + " | escMD }}";
    },
    "cond":   function () {
      return "{{ if eq .level \"critical\" }}🚨{{ end }}";
    },
    "loop":   function () {
      return "{{ range .items }}\n  - {{ . | escMD }}\n{{ end }}";
    },
    "link":   function () {
      return "[查看详情]({{ .url }})";
    },
    "code":   function () {
      return "```\n{{ .code }}\n```";
    },
    "escape": null  // handled separately — wraps the current selection
  };

  function insertAtCaret(textarea, text) {
    var start = textarea.selectionStart || 0;
    var end   = textarea.selectionEnd   || 0;
    var v = textarea.value;
    textarea.value = v.slice(0, start) + text + v.slice(end);
    textarea.selectionStart = textarea.selectionEnd = start + text.length;
    textarea.focus();
  }

  function wrapSelection(textarea, before, after) {
    var start = textarea.selectionStart || 0;
    var end   = textarea.selectionEnd   || 0;
    var v = textarea.value;
    var sel = v.slice(start, end);
    if (!sel) {
      window.alert("请先选中要包裹的内容");
      return;
    }
    var newText = before + sel + after;
    textarea.value = v.slice(0, start) + newText + v.slice(end);
    textarea.selectionStart = start;
    textarea.selectionEnd = start + newText.length;
    textarea.focus();
  }

  function bindToolbar(textarea) {
    $all("[data-tpl-insert]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var kind = btn.getAttribute("data-tpl-insert");
        if (kind === "escape") {
          wrapSelection(textarea, "{{ ", " | escMD }}");
          return;
        }
        var fn = snippets[kind];
        if (!fn) return;
        var txt = fn();
        if (txt == null) return;
        insertAtCaret(textarea, txt);
      });
    });
  }

  function bindDemos(textarea) {
    $all("[data-tpl-demo]").forEach(function (btn) {
      btn.addEventListener("click", function () {
        var card = btn.closest("div");
        if (!card) return;
        var pre = card.querySelector("pre");
        if (!pre) return;
        var body = pre.textContent || "";
        if (textarea.value.trim() && !window.confirm("当前编辑器有内容，是否覆盖？")) {
          return;
        }
        textarea.value = body;
        textarea.focus();
      });
    });
  }

  document.addEventListener("DOMContentLoaded", function () {
    var textarea = $("#tplBody");
    if (!textarea) return;
    bindToolbar(textarea);
    bindDemos(textarea);
  });
})();
