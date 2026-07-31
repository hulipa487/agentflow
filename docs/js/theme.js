// Theme toggle — persists to localStorage
(function () {
  const root = document.documentElement;
  const stored = localStorage.getItem('af-theme');
  if (stored) {
    root.setAttribute('data-theme', stored);
  }
  const btn = document.getElementById('theme-toggle');
  const iconLight = btn.querySelector('.theme-icon-light');
  const iconDark = btn.querySelector('.theme-icon-dark');

  function syncIcons() {
    const theme = root.getAttribute('data-theme');
    iconLight.hidden = theme === 'light';
    iconDark.hidden = theme === 'dark';
  }
  syncIcons();

  btn.addEventListener('click', function () {
    const current = root.getAttribute('data-theme');
    const next = current === 'dark' ? 'light' : 'dark';
    root.setAttribute('data-theme', next);
    localStorage.setItem('af-theme', next);
    syncIcons();
  });
})();
