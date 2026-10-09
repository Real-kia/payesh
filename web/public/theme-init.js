// Applies saved appearance choices before the dashboard bundle loads so the
// first paint already uses the right theme. Kept tiny and dependency-free.
(function () {
  var root = document.documentElement;
  var get = function (key) { try { return window.localStorage.getItem(key); } catch (e) { return null; } };
  var mode = get('payesh-theme');
  var dark = mode === 'dark' || ((mode !== 'light') && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
  root.setAttribute('data-theme', dark ? 'dark' : 'light');
  var accent = get('payesh-accent');
  if (accent === 'teal' || accent === 'violet' || accent === 'amber') root.setAttribute('data-accent', accent);
  if (get('payesh-motion') === 'reduced') root.setAttribute('data-motion', 'reduced');
  if (get('payesh-density') === 'compact') root.setAttribute('data-density', 'compact');
})();
