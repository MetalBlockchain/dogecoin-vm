// Apply a saved theme before first paint, so the page does not flash. This
// is a separate file because the page's content security policy allows no
// inline scripts.
try {
  const t = localStorage.getItem('dogevm.theme');
  if (t === 'dark' || t === 'system') document.documentElement.dataset.theme = t;
} catch { /* storage unavailable: light */ }
