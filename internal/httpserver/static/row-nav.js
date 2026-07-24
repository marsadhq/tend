// row-nav.js makes ".row-link" table rows clickable without inline onclick
// attributes, which a strict Content-Security-Policy (script-src 'self', no
// 'unsafe-inline') blocks. Delegated on document so it keeps working after
// htmx swaps rows in.
document.addEventListener("click", function (e) {
  var row = e.target.closest(".row-link");
  if (!row || e.target.closest(".col-actions")) return;
  var href = row.dataset.href;
  if (href) location.href = href;
});
