// Progressive enhancement: a form with data-confirm asks before submitting. Kept in
// a self-hosted file (not an inline handler) so the panel's CSP can forbid inline
// script. With JS off, forms still submit — the server-side guards are the real
// protection; this is only a courtesy prompt.
(function () {
  document.addEventListener("submit", function (e) {
    var form = e.target;
    if (!form || !form.getAttribute) return;
    var msg = form.getAttribute("data-confirm");
    if (msg && !window.confirm(msg)) {
      e.preventDefault();
    }
  });
})();
