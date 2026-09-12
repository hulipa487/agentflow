// AgentFlow shared theme toggle — dark default, persists to localStorage.
// Used by both the console and the docs site. The toggle button is optional:
// any element with id="theme-toggle" gets wired; icons with the classes
// .theme-icon-light / .theme-icon-dark are swapped if present.
(function () {
  var root = document.documentElement;
  var stored = null;
  try { stored = localStorage.getItem('af-theme'); } catch (e) {}
  root.setAttribute('data-theme', stored === 'light' ? 'light' : 'dark');

  function syncIcons(btn) {
    var theme = root.getAttribute('data-theme');
    var light = btn.querySelector('.theme-icon-light');
    var dark = btn.querySelector('.theme-icon-dark');
    if (light) light.hidden = theme === 'light';
    if (dark) dark.hidden = theme === 'dark';
    // Text-only fallback (no icon spans): show the target theme.
    if (!light && !dark) btn.textContent = theme === 'dark' ? '☀️' : '🌙';
  }

  function wire() {
    var btn = document.getElementById('theme-toggle');
    if (!btn) return;
    syncIcons(btn);
    btn.addEventListener('click', function () {
      var next = root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
      root.setAttribute('data-theme', next);
      try { localStorage.setItem('af-theme', next); } catch (e) {}
      syncIcons(btn);
    });
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', wire);
  } else {
    wire();
  }
})();
