// Checks the web wallet's amount units (TestWebUnits): DOGE and USD show
// and read back as exact koinu, and dollars need a fresh price. Prints "ok".
import assert from 'node:assert/strict';
import * as units from '../web/units.js';
import { KOINU } from '../web/chain.js';

const now = 1_800_000_000_000;
const price = { usd: 0.098535, time: now - 60_000 }; // $0.098535 a DOGE, a minute old
const stale = { usd: 0.098535, time: now - units.PRICE_MAX_AGE - 1 };

// Showing.
assert.equal(units.format(1_250_000_000n, 'doge'), '12.5 DOGE');
assert.equal(units.format(0n, 'doge'), '0 DOGE');
assert.equal(units.format(123_456_789_000_000n, 'doge'), '1,234,567.89 DOGE');
assert.equal(units.format(100n * KOINU, 'usd', price, now), '$9.85');
assert.equal(units.format(KOINU, 'usd', price, now), '$0.10');
assert.equal(units.format(1_000_000n * KOINU, 'usd', price, now), '$98,535.00');
assert.equal(units.format(1n, 'usd', price, now), '< $0.01');
assert.equal(units.format(0n, 'usd', price, now), '$0.00');
// Without a fresh price, USD shows DOGE rather than an old figure.
assert.equal(units.format(100n * KOINU, 'usd', stale, now), '100 DOGE');
assert.equal(units.format(100n * KOINU, 'usd', null, now), '100 DOGE');
assert.deepEqual(units.parts(100n * KOINU, 'doge'), { value: '100', unit: 'DOGE' });
assert.deepEqual(units.parts(100n * KOINU, 'usd', price, now), { value: '$9.85', unit: '' });
assert.equal(units.formatPrice(price), '$0.098535');
assert.equal(units.formatPrice({ usd: 0.1 }), '$0.10');
assert.equal(units.formatPrice({ usd: 1234.5 }), '$1,234.50');

// Reading.
assert.equal(units.parse('12.5', 'doge'), 1_250_000_000n);
assert.equal(units.parse('0.00000001', 'doge'), 1n);
assert.equal(units.parse(' 1,000 DOGE ', 'doge'), 1000n * KOINU);
assert.throws(() => units.parse('0.000000001', 'doge'));
assert.throws(() => units.parse('-5', 'doge'));
assert.throws(() => units.parse('', 'doge'));
assert.throws(() => units.parse('10000000001', 'doge'), /10 billion/);
// $1 at $0.098535 is 10.14867… DOGE: it rounds to the nearest koinu.
assert.equal(units.parse('1', 'usd', price, now), 1_014_867_813n);
assert.equal(units.parse('$1.00', 'usd', price, now), 1_014_867_813n);
assert.equal(units.parse('98,535', 'usd', price, now), 1_000_000n * KOINU);
assert.throws(() => units.parse('1.005', 'usd', price, now));
assert.throws(() => units.parse('1', 'usd', stale, now), /price/);
assert.throws(() => units.parse('1', 'usd', null, now), /price/);
assert.throws(() => units.parse('1', 'usd', { usd: 0.098535, time: now + 60_000 }, now), /price/);
assert.throws(() => units.parse('1', 'usd', { usd: NaN, time: now }, now), /price/);
assert.throws(() => units.parse('1', 'usd', { usd: 0, time: now }, now), /price/);
assert.throws(() => units.parse('999999999999', 'usd', price, now), /10 billion/);

// A price exactly at the limit is still used; a millisecond later it isn't.
assert.ok(units.freshPrice({ usd: 0.1, time: now - units.PRICE_MAX_AGE }, now));
assert.equal(units.freshPrice({ usd: 0.1, time: now - units.PRICE_MAX_AGE - 1 }, now), null);

// Every amount shown in DOGE reads back exactly, and a dollar amount shown
// reads back to within half a cent.
for (const n of [1n, 100_000n, KOINU, 123_456_789n, 10_000_000_000n * KOINU]) {
  assert.equal(units.parse(units.format(n, 'doge'), 'doge'), n);
}
for (const n of [KOINU, 250n * KOINU, 123_456n * KOINU]) {
  const back = units.parse(units.format(n, 'usd', price, now), 'usd', price, now);
  const diff = back > n ? back - n : n - back;
  assert.ok(diff * 98_535n <= KOINU * 5_000n, `${n} came back as ${back}`);
}

assert.equal(units.exact(1_250_000_000n), '12.5 DOGE');
console.log('ok');
