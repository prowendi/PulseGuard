// Glue between htmx and PulseGuard's CSRF cookie. We read psg_csrf from
// document.cookie and copy it into the X-CSRF-Token header before every
// state-mutating htmx request. Pages without htmx loaded fall back to plain
// form posts that go through middleware/csrf.go on the server.
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
  document.body && document.body.addEventListener("htmx:configRequest", function (evt) {
    var token = readCookie("psg_csrf");
    if (token) evt.detail.headers["X-CSRF-Token"] = token;
  });
})();
