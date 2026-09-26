// Checks the web wallet's address book (TestWebAddressBook): only addresses
// the forms' own decoder accepts are saved, names are cleaned and capped,
// the book is capped, stored data is re-checked, and lookups find an
// address for its network. Prints "ok".
//
// Test keys are sha256 of public labels: this file holds no secret.
import assert from 'node:assert/strict';
import * as chain from '../web/chain.js';
import * as book from '../web/addressbook.js';
import { sha256 } from '../web/vendor/noble-hashes-1.8.0/sha2.js';

// Mainnet: Dogecoin and DogecoinVM share address formats. Testnet stands
// in for a network whose addresses differ.
const mainnet = { p2pkh: 30, p2sh: 22, wif: 158 };
const testnet = { p2pkh: 113, p2sh: 196, wif: 241 };
const versions = { doge: mainnet, vm: mainnet };
const decode = (a, n) => chain.encodeAddress(chain.decodeAddress(a, versions[n]), versions[n]);

const dest = (label) => chain.keyDestination(sha256(new TextEncoder().encode(label)));
const legacy = chain.encodeAddress(dest('dogevm book mum'), mainnet); // D…
const script = chain.encodeAddress({ kind: 1, hash: dest('dogevm book kraken').hash }, mainnet); // 9… or A…
const onTestnet = chain.encodeAddress(dest('dogevm book testnet'), testnet);
assert.match(legacy, /^D/);
assert.match(script, /^[9A]/);

// Adding.
let list = [];
list = book.add(list, { name: '  Mum ', address: legacy, network: 'doge' }, decode);
assert.deepEqual(list, [{ name: 'Mum', address: legacy, network: 'doge' }]);
// Saved in canonical form: surrounding space is dropped.
list = book.add(list, { name: 'Kraken deposit', address: ` ${script}\n`, network: 'doge' }, decode);
assert.equal(list[1].address, script);
// The same address may be saved for the other network, not twice for one.
list = book.add(list, { name: 'Mum on DogecoinVM', address: legacy, network: 'vm' }, decode);
assert.equal(list.length, 3);
assert.throws(() => book.add(list, { name: 'Again', address: legacy, network: 'doge' }, decode), /Already saved as "Mum"/);

// Never an invalid address.
const typo = legacy.slice(0, -1) + (legacy.endsWith('a') ? 'b' : 'a');
assert.throws(() => book.add(list, { name: 'Typo', address: typo, network: 'doge' }, decode), /Not a valid Dogecoin address/);
assert.throws(() => book.add(list, { name: 'Testnet', address: onTestnet, network: 'doge' }, decode), /different network/);
// A Bitcoin address has the same form but not the same version byte.
const onBitcoin = chain.encodeAddress(dest('dogevm book bitcoin'), { p2pkh: 0, p2sh: 5 });
assert.throws(() => book.add(list, { name: 'Bitcoin', address: onBitcoin, network: 'vm' }, decode), /Not a valid DogecoinVM address: address is for a different network/);
assert.throws(() => book.add(list, { name: 'Empty', address: '  ', network: 'doge' }, decode), /Enter an address/);
assert.throws(() => book.add(list, { name: 'Nowhere', address: script, network: 'btc' }, decode), /Dogecoin or DogecoinVM/);
assert.throws(() => book.add(list, { name: 'Proto', address: script, network: '__proto__' }, decode), /Dogecoin or DogecoinVM/);
assert.equal(list.length, 3);

// Names: cleaned, required, capped at 60 characters (not UTF-16 units).
assert.equal(book.cleanName(' Kraken \n\t deposit '), 'Kraken deposit');
assert.equal(book.cleanName('Mu\u202Em\u200B'), 'Mum'); // bidi override and zero-width removed
assert.throws(() => book.cleanName('   '), /name/);
assert.throws(() => book.cleanName('\u200B\u202E'), /name/);
assert.equal(book.cleanName('x'.repeat(60)), 'x'.repeat(60));
assert.throws(() => book.cleanName('x'.repeat(61)), /at most 60/);
assert.equal(book.cleanName('😀'.repeat(60)), '😀'.repeat(60));
assert.throws(() => book.add(list, { name: 'y'.repeat(61), address: script, network: 'vm' }, decode), /at most 60/);
// A name is plain text: markup is kept as typed, for textContent to show.
assert.equal(book.cleanName('<img src=x onerror=alert(1)>'), '<img src=x onerror=alert(1)>');

// Renaming and deleting.
list = book.rename(list, legacy, 'doge', '  Mum (savings) ');
assert.equal(book.find(list, legacy, 'doge').name, 'Mum (savings)');
assert.equal(book.find(list, legacy, 'vm').name, 'Mum on DogecoinVM');
assert.throws(() => book.rename(list, legacy, 'doge', ''), /name/);
assert.throws(() => book.rename(list, script, 'vm', 'Nope'), /no longer/);
list = book.remove(list, legacy, 'vm');
assert.equal(list.length, 2);

// Lookup: the entry for the network, else one for the other network.
assert.equal(book.find(list, script, 'doge').name, 'Kraken deposit');
const other = book.find(list, script, 'vm');
assert.equal(other.network, 'doge'); // the caller says it was saved for Dogecoin
assert.equal(book.find(list, chain.encodeAddress(dest('dogevm book stranger'), mainnet), 'doge'), null);

// Matching what is typed: by name or address, ignoring case; one network
// only, or the preferred network first.
list = book.add(list, { name: 'Alice', address: script, network: 'vm' }, decode);
assert.deepEqual(book.matches(list, 'KRAK').map((e) => e.name), ['Kraken deposit']);
assert.deepEqual(book.matches(list, script.slice(4, 14)).map((e) => e.name), ['Alice', 'Kraken deposit']);
assert.deepEqual(book.matches(list, '', { only: 'doge' }).map((e) => e.name), ['Kraken deposit', 'Mum (savings)']);
assert.deepEqual(book.matches(list, '', { prefer: 'doge' }).map((e) => e.name), ['Kraken deposit', 'Mum (savings)', 'Alice']);
assert.deepEqual(book.matches(list, 'nobody'), []);
assert.equal(book.shorten(script), `${script.slice(0, 10)}…${script.slice(-8)}`);

// The cap.
let full = [];
for (let i = 0; full.length < book.MAX_ENTRIES; i++) {
  full = book.add(full, { name: `n${i}`, address: chain.encodeAddress(dest(`dogevm book ${i}`), mainnet), network: 'doge' }, decode);
}
assert.throws(() => book.add(full, { name: 'one more', address: script, network: 'doge' }, decode), /full/);

// Stored data round-trips, and is re-checked as it loads: bad entries,
// duplicates and anything past the cap are dropped.
assert.deepEqual(book.parse(book.serialize(list), decode), list);
const stored = JSON.stringify([
  { name: 'Good', address: legacy, network: 'doge' },
  { name: 'Dup', address: legacy, network: 'doge' },
  { name: 'Bad address', address: typo, network: 'doge' },
  { name: '', address: script, network: 'doge' },
  { name: 'Bad network', address: script, network: 'btc' },
  { name: 'x'.repeat(61), address: script, network: 'vm' },
  { name: 'Extra fields dropped', address: script, network: 'doge', html: '<b>' },
  null, 7, 'text',
]);
assert.deepEqual(book.parse(stored, decode), [
  { name: 'Good', address: legacy, network: 'doge' },
  { name: 'Extra fields dropped', address: script, network: 'doge' },
]);
assert.deepEqual(book.parse(null, decode), []);
assert.deepEqual(book.parse('not json', decode), []);
assert.deepEqual(book.parse('{"a":1}', decode), []);
assert.equal(book.parse(JSON.stringify([...full, { name: 'over', address: script, network: 'vm' }]), decode).length, book.MAX_ENTRIES);

console.log('ok');
