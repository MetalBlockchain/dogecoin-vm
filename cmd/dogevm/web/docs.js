// Fills the docs page's figures from the bridge, so they're never out of date.
import { describeTiers } from './tiers.js';
const tidy = (s) => {
  const [whole, frac = ''] = String(s).split('.');
  const f = frac.replace(/0+$/, '');
  return Number(whole).toLocaleString('en-US') + (f ? '.' + f : '');
};

try {
  const res = await fetch('/api/info');
  const info = await res.json();
  for (const el of document.querySelectorAll('[data-info]')) {
    const key = el.dataset.info;
    let v = info[key];
    if (key === 'signers') v = `${info.signers.required} of ${info.signers.publicKeys.length}`;
    else if (key === 'confirmationTiers') v = describeTiers(info);
    else if ('doge' in el.dataset) v = tidy(v) === '0' && key.startsWith('max') ? 'no limit' : tidy(v);
    if (v !== undefined) el.textContent = v;
  }
} catch {
  for (const el of document.querySelectorAll('[data-info]')) el.textContent = 'unavailable';
}
