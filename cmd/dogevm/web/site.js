// What every page shares: the theme toggle in the footer. Light unless the
// viewer picks dark or their system setting; theme.js applies the saved
// choice before first paint.
const THEME_STORE = 'dogevm.theme';
const themes = ['light', 'dark', 'system'];
const button = document.getElementById('theme-toggle');

function read() {
  try { return localStorage.getItem(THEME_STORE); } catch { return null; }
}

function apply(theme) {
  if (theme === 'light') document.documentElement.removeAttribute('data-theme');
  else document.documentElement.setAttribute('data-theme', theme);
  if (button) button.textContent = `Theme: ${theme}`;
}

apply(themes.includes(read()) ? read() : 'light');
button?.addEventListener('click', () => {
  const current = document.documentElement.getAttribute('data-theme') || 'light';
  const next = themes[(themes.indexOf(current) + 1) % themes.length];
  try { localStorage.setItem(THEME_STORE, next); } catch { /* not kept */ }
  apply(next);
});
