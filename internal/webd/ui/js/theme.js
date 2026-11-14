// Applied before the first paint, so the page never flashes the wrong
// theme. Kept out of the module graph for that reason.
(function () {
  var t = null;
  try { t = localStorage.getItem('trellis.theme'); } catch (e) { /* private mode */ }
  if (t !== 'dusk' && t !== 'day' && t !== 'night') {
    t = window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches ? 'day' : 'dusk';
  }
  document.documentElement.setAttribute('data-theme', t);
})();
