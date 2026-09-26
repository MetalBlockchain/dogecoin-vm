// Amounts shown and entered in DOGE or US dollars. Amounts are BigInt koinu
// throughout. A dollar amount is converted at a price passed in explicitly,
// and rounds to whole koinu; the wallet signs only the koinu, never a dollar
// figure.
//
// There is no koinu unit: a koinu is a hundred-millionth of a DOGE, and no
// payment is smaller than 0.001 DOGE (Dogecoin's hard dust limit), so DOGE
// with decimals covers every amount a wallet can send.
import { KOINU, parseDoge, formatDoge } from './chain.js';

export const UNITS = ['doge', 'usd'];

// A price older than this is not used, to show or to enter amounts.
export const PRICE_MAX_AGE = 10 * 60 * 1000;

// Dogecoin Core's MAX_MONEY: no single amount can be larger.
const MAX_KOINU = 10_000_000_000n * KOINU;

// A DOGE is worth cents, not thousands of dollars, so the price is kept to
// a hundred-millionth of a dollar: integer cents per DOGE would round
// $0.0985 to $0.10, 1.5% off.
const SCALE = 100_000_000n; // price units per dollar
const PER_CENT = SCALE / 100n;

// freshPrice returns price ({usd, time}: dollars per DOGE, and when this
// page fetched it, in ms) if it is recent enough to convert with, or null.
export function freshPrice(price, now = Date.now()) {
  if (!price || !(price.usd > 0) || !Number.isFinite(price.usd)) return null;
  const age = now - price.time;
  return age >= 0 && age <= PRICE_MAX_AGE ? price : null;
}

// The price in hundred-millionths of a dollar per DOGE, as an integer.
const scaled = (price) => BigInt(Math.round(price.usd * Number(SCALE)));

const dollars = (cents) =>
  `$${(cents / 100n).toLocaleString('en-US')}.${(cents % 100n).toString().padStart(2, '0')}`;

// parts splits an amount into its figure and its unit label, for places
// that style the two apart. Without a usable price, USD falls back to DOGE.
export function parts(koinu, unit, price, now = Date.now()) {
  koinu = BigInt(koinu);
  const p = unit === 'usd' ? freshPrice(price, now) : null;
  if (!p) return { value: formatDoge(koinu), unit: 'DOGE' };
  const per = scaled(p);
  const cents = (koinu * per + (KOINU * PER_CENT) / 2n) / (KOINU * PER_CENT);
  if (koinu > 0n && cents === 0n) return { value: '< $0.01', unit: '' };
  return { value: dollars(cents), unit: '' };
}

// format shows an amount in a unit: "12.5 DOGE", "$1.23".
export function format(koinu, unit, price, now = Date.now()) {
  const p = parts(koinu, unit, price, now);
  return p.unit ? `${p.value} ${p.unit}` : p.value;
}

// formatPrice shows the price of one DOGE to up to six decimals, since a
// DOGE is worth cents: "$0.098535".
export function formatPrice(price) {
  const text = price.usd.toFixed(6).replace(/0+$/, '');
  const [whole, frac = ''] = text.split('.');
  return `$${Number(whole).toLocaleString('en-US')}.${frac.padEnd(2, '0')}`;
}

// parse reads an amount typed in a unit and returns its exact koinu. DOGE
// has up to 8 decimals and dollars up to 2; dollars need a fresh price and
// round to the nearest koinu.
export function parse(text, unit, price, now = Date.now()) {
  const s = String(text).trim();
  let koinu;
  if (unit === 'usd') {
    const p = freshPrice(price, now);
    if (!p) throw new Error("The DOGE price isn't available right now, so amounts can't be entered in USD. Switch to DOGE.");
    const m = /^\$?\s*(\d{1,12})(?:\.(\d{1,2}))?$/.exec(s.replace(/,/g, ''));
    if (!m) throw new Error('enter an amount like 1.25');
    const cents = BigInt(m[1]) * 100n + BigInt((m[2] || '').padEnd(2, '0'));
    const per = scaled(p);
    koinu = (cents * PER_CENT * KOINU + per / 2n) / per;
  } else {
    koinu = parseDoge(s.replace(/\s*doge$/i, '').replace(/,/g, ''));
  }
  if (koinu > MAX_KOINU) throw new Error('more than 10 billion DOGE, the most one payment can carry');
  return koinu;
}

// exact shows an amount in DOGE, for the line under an amount field: what
// is actually sent.
export const exact = (koinu) => `${formatDoge(koinu)} DOGE`;
