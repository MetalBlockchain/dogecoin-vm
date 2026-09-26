// The address book: names for addresses the viewer pays, kept in this
// browser only. This module is the book's logic and holds no page state, so
// it runs under Node too (testdata/addressbook_check.mjs).
//
// An entry is {name, address, network}: network is 'doge' (Dogecoin) or
// 'vm' (DogecoinVM). On mainnet the two share address formats, so the
// network is what the viewer said the address is for, not something read
// from it.
//
// Addresses are checked with decode(address, network), which returns the
// address's canonical text or throws: the page passes chain.js's decoder,
// the same one its forms use. Nothing that fails it is ever stored.

export const MAX_NAME = 60; // characters
export const MAX_ENTRIES = 200;
export const NETWORKS = { doge: 'Dogecoin', vm: 'DogecoinVM' };

// Control, zero-width and bidirectional-override characters: a name is
// shown next to an address, so it must not be able to hide or reorder text.
const HIDDEN = /[\u0000-\u001f\u007f-\u009f\u00ad\u061c\u180e\u200b-\u200f\u2028-\u202e\u2060-\u206f\ufeff]/g;

// cleanName returns a name as it is stored: hidden characters removed,
// whitespace collapsed. It throws if nothing is left or it is too long.
export function cleanName(name) {
  const s = String(name ?? '').replace(HIDDEN, '').replace(/\s+/g, ' ').trim();
  if (!s) throw new Error('Give the address a name.');
  if ([...s].length > MAX_NAME) throw new Error(`A name can be at most ${MAX_NAME} characters.`);
  return s;
}

function checkNetwork(network) {
  if (!Object.hasOwn(NETWORKS, network)) throw new Error('Choose Dogecoin or DogecoinVM.');
  return network;
}

// canonical returns the address's canonical text for a network, or throws
// with a reason fit to show.
export function canonical(address, network, decode) {
  checkNetwork(network);
  const text = String(address ?? '').trim();
  if (!text) throw new Error('Enter an address.');
  try {
    return decode(text, network);
  } catch (err) {
    throw new Error(`Not a valid ${NETWORKS[network]} address: ${err.message}.`);
  }
}

const same = (e, address, network) => e.address === address && e.network === network;

// add returns the list with a new entry, or throws why it can't be saved.
export function add(list, entry, decode) {
  const name = cleanName(entry.name);
  const network = checkNetwork(entry.network);
  const address = canonical(entry.address, network, decode);
  const dup = list.find((e) => same(e, address, network));
  if (dup) throw new Error(`Already saved as "${dup.name}".`);
  if (list.length >= MAX_ENTRIES) throw new Error(`The address book is full: it holds ${MAX_ENTRIES} addresses. Delete one first.`);
  return [...list, { name, address, network }];
}

export function rename(list, address, network, name) {
  const clean = cleanName(name);
  if (!list.some((e) => same(e, address, network))) throw new Error('That address is no longer in the book.');
  return list.map((e) => (same(e, address, network) ? { ...e, name: clean } : e));
}

export const remove = (list, address, network) => list.filter((e) => !same(e, address, network));

// find returns the entry for a canonical address: the one saved for network
// if there is one, else one saved for the other network (the caller should
// say so), else null.
export function find(list, address, network) {
  return list.find((e) => same(e, address, network)) || list.find((e) => e.address === address) || null;
}

// matches lists the entries to offer for what has been typed: those whose
// name or address contains it, ignoring case. only limits them to one
// network; otherwise entries for prefer come first. Within that, by name.
export function matches(list, query, { only, prefer } = {}) {
  const q = String(query ?? '').trim().toLowerCase();
  return list
    .filter((e) => (!only || e.network === only)
      && (!q || e.name.toLowerCase().includes(q) || e.address.toLowerCase().includes(q)))
    .sort((a, b) => (Number(b.network === prefer) - Number(a.network === prefer)) || byName(a, b));
}

export const byName = (a, b) => a.name.localeCompare(b.name, undefined, { sensitivity: 'base' })
  || a.address.localeCompare(b.address);

// shorten keeps an address's start and end, for a list; the full address is
// always shown wherever a payment is checked.
export const shorten = (address) => (address.length > 22 ? `${address.slice(0, 10)}…${address.slice(-8)}` : address);

export const serialize = (list) => JSON.stringify(list.map(({ name, address, network }) => ({ name, address, network })));

// parse reads a stored book. Stored data is re-checked as if typed:
// anything malformed, invalid for its network, duplicated or over the cap
// is dropped rather than trusted.
export function parse(text, decode) {
  let raw;
  try { raw = JSON.parse(text || '[]'); } catch { return []; }
  if (!Array.isArray(raw)) return [];
  let list = [];
  for (const e of raw) {
    if (!e || typeof e !== 'object') continue;
    try {
      list = add(list, e, decode);
    } catch { /* dropped */ }
  }
  return list;
}
