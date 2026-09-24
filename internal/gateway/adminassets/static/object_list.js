// Progressive enhancement for object_list editors: clone a hidden <template> row to
// add, and drop a row to remove. Row indices only need to be unique within the form
// (the server collects whatever indices are present and ignores gaps), so a simple
// ever-increasing counter is enough. Self-hosted so the panel's CSP needs no inline
// script. With JS off, existing rows still render and submit — you just can't add or
// remove.
(function () {
  document.addEventListener("click", function (e) {
    var t = e.target;
    if (!t || !t.closest) return;

    var add = t.closest("[data-ol-add]");
    if (add) {
      e.preventDefault();
      var wrap = add.closest("[data-ol]");
      if (!wrap) return;
      var tmpl = wrap.querySelector("template[data-ol-template]");
      var rows = wrap.querySelector("[data-ol-rows]");
      if (!tmpl || !rows) return;
      var n = parseInt(wrap.getAttribute("data-ol-count") || "0", 10) || 0;
      var html = tmpl.innerHTML.replace(/__IDX__/g, String(n));
      var holder = document.createElement("div");
      holder.innerHTML = html.trim();
      var row = holder.firstElementChild;
      if (row) {
        rows.appendChild(row);
        wrap.setAttribute("data-ol-count", String(n + 1));
      }
      return;
    }

    var rem = t.closest("[data-ol-remove]");
    if (rem) {
      e.preventDefault();
      var row = rem.closest("[data-ol-row]");
      if (row) row.remove();
    }
  });
})();
