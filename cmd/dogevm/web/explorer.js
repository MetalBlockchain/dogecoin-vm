// The explorer: bridge activity, proof of reserves, DogecoinVM blocks, and
// pages for a transaction, block or address. Pages are routed by the URL
// hash (#/tx/<id>, #/block/<id>, #/address/<address>) so they can be linked.
import * as chain from './chain.js';

const $ = (id) => document.getElementById(id);
let info = null;

// route counts page changes, so a slow response for a page the viewer has
// left does not replace the one they are on.
let routeGen = 0;

async function api(path) {
  const res = await fetch(path);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw Object.assign(new Error(data.error || `request failed (${res.status})`), { status: res.status });
  return data;
}

const tidy = (s) => chain.formatDoge(chain.parseDoge(String(s)));
const short = (id) => `${id.slice(0, 8)}…${id.slice(-6)}`;

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') node.className = v;
    else node.setAttribute(k, v);
  }
  for (const c of children) if (c !== null && c !== undefined) node.append(c);
  return node;
}

function ago(unix) {
  if (!unix) return '';
  const s = Math.max(0, Math.round(Date.now() / 1000 - unix));
  if (s < 90) return `${s}s ago`;
  if (s < 5400) return `${Math.round(s / 60)} min ago`;
  if (s < 129600) return `${Math.round(s / 3600)} h ago`;
  const days = Math.round(s / 86400);
  return `${days} day${days === 1 ? '' : 's'} ago`;
}

// Links: Dogecoin ids open on a public explorer, DogecoinVM ids in-page.
// The explorers are fixed here, not taken from the server, and ids are
// checked before they go into a URL.
const DOGE_EXPLORERS = {
  mainnet: { tx: 'https://blockchair.com/dogecoin/transaction/', address: 'https://blockchair.com/dogecoin/address/' },
  testnet: { tx: 'https://sochain.com/tx/DOGETEST/', address: 'https://sochain.com/address/DOGETEST/' },
};
const isTxid = (s) => /^[0-9a-f]{64}$/.test(s);
const isAddress = (s) => /^[1-9A-HJ-NP-Za-km-z]{25,40}$/.test(s);

function dogeLink(kind, id, text) {
  const base = (DOGE_EXPLORERS[info.dogecoinNetwork] || {})[kind];
  const ok = kind === 'tx' ? isTxid(id) : isAddress(id);
  return base && ok ? el('a', { href: base + id, target: '_blank', rel: 'noopener noreferrer', class: 'chain-doge' }, text) : text;
}
const dogeTx = (txid) => dogeLink('tx', txid, el('code', {}, short(txid)));
const vmTx = (txid) => el('a', { href: `#/tx/${txid}`, class: 'chain-vm' }, el('code', {}, short(txid)));
const vmAddress = (addr) => el('a', { href: `#/address/${addr}`, class: 'chain-vm' }, el('code', {}, addr));
const vmBlock = (h) => el('a', { href: `#/block/${h}`, class: 'chain-vm' }, `#${Number(h).toLocaleString('en-US')}`);

// --- home: activity, reserves, blocks --------------------------------------

function renderMove(e) {
  const done = e.status === 'credited' || e.status === 'paid';
  const statusText = {
    credited: `Credited ${e.credited ? tidy(e.credited) : ''} DOGE`,
    paid: `Paid ${e.pays ? tidy(e.pays) : ''} DOGE`,
    waiting: e.type === 'deposit'
      ? `${Math.min(e.confirmations ?? 0, e.required ?? 0)} of ${e.required} confirmations`
      : 'Waiting for the bridge',
    held: 'Held for a refund',
    refunded: 'Refunded',
  }[e.status] || e.status;

  let from;
  let to;
  if (e.type === 'deposit') {
    from = el('div', { class: 'side side-doge' },
      el('span', { class: 'side-label' }, 'Dogecoin'), dogeTx(e.dogecoinTxid));
    to = el('div', { class: 'side side-vm' },
      el('span', { class: 'side-label' }, 'DogecoinVM'),
      e.creditTxid ? vmTx(e.creditTxid) : el('span', { class: 'pending' }, 'not yet'));
  } else {
    from = el('div', { class: 'side side-vm' },
      el('span', { class: 'side-label' }, 'DogecoinVM'), vmTx(e.dogecoinvmTxid));
    to = el('div', { class: 'side side-doge' },
      el('span', { class: 'side-label' }, 'Dogecoin'),
      e.dogecoinTxid ? dogeTx(e.dogecoinTxid) : el('span', { class: 'pending' }, 'not yet'));
  }
  return el('li', { class: `move move-${e.type}` },
    el('div', { class: 'move-head' },
      el('span', { class: 'move-kind' }, e.type === 'deposit' ? 'Deposit' : 'Withdrawal'),
      el('span', { class: 'amount move-amount' }, e.amount ? `${tidy(e.amount)} DOGE` : ''),
      el('span', { class: 'move-time' }, ago(e.time))),
    el('div', { class: 'move-path' }, from, el('span', { class: 'arrow', 'aria-hidden': 'true' }, '→'), to),
    el('p', { class: `move-status ${done ? 'status-done' : 'status-waiting'}` }, statusText));
}

// unchanged reports whether a list's data is the same as last time, so
// polling does not rebuild it (and lose a reader's place or selection).
const lastData = {};
function unchanged(name, data) {
  const json = JSON.stringify(data);
  if (lastData[name] === json) return true;
  lastData[name] = json;
  return false;
}

async function refreshActivity() {
  try {
    const events = await api('/api/activity');
    if (unchanged('activity', events)) return;
    const list = $('activity');
    list.replaceChildren(...(events.length ? events.map(renderMove)
      : [el('li', { class: 'empty' }, 'Nothing has crossed the bridge yet.')]));
  } catch { /* next poll */ }
}

async function refreshReserves() {
  try {
    const r = await api('/api/reserves');
    if (unchanged('reserves', r)) return;
    $('res-locked').textContent = `${tidy(r.lockedOnDogecoin)} DOGE`;
    $('res-circulating').textContent = `${tidy(r.circulating)} DOGE`;
    const list = $('reserve-outputs');
    list.replaceChildren(...r.dogecoinOutputs.map((o) =>
      el('li', {},
        dogeLink('tx', o.txid, el('code', {}, `${short(o.txid)}:${o.vout}`)),
        el('span', { class: 'muted' }, `${Number(o.confirmations).toLocaleString('en-US')} conf.`),
        el('span', { class: 'amount' }, `${tidy(o.amount)} DOGE`))));
    if (!r.dogecoinOutputs.length) list.append(el('li', { class: 'empty' }, 'No DOGE is locked yet.'));
  } catch { /* next poll */ }
}

async function refreshBlocks() {
  try {
    const { blocks } = await api('/api/blocks');
    if (unchanged('blocks', blocks.slice(0, 8))) return;
    $('blocks').replaceChildren(...blocks.slice(0, 8).map((b) =>
      el('li', {}, vmBlock(b.height),
        el('span', {}, `${b.txCount} transaction${b.txCount === 1 ? '' : 's'}`),
        el('span', { class: 'move-time' }, ago(b.time)))));
  } catch { /* next poll */ }
}

// --- pages ---------------------------------------------------------------------

// page shows a page, if the viewer is still on the route that asked for
// it. The final render moves focus to the heading, so screen readers
// announce the new page.
function page(gen, title, ...body) {
  if (gen !== routeGen) return;
  document.title = `${title}: DogecoinVM explorer`;
  const heading = el('h2', { tabindex: '-1' }, title);
  $('explorer-view').replaceChildren(
    el('p', {}, el('a', { href: '#' }, '← Back to the explorer')),
    heading, ...body.filter((b) => b !== null && b !== undefined));
  return heading;
}
const loading = (gen, title) => page(gen, title, el('p', { role: 'status' }, 'Loading…'));
const done = (heading) => heading && heading.focus();

function ioTable(rows) {
  return el('ul', { class: 'io' }, ...rows.map((r) => el('li', {},
    r.address ? vmAddress(r.address) : el('span', { class: 'muted' }, { 'bridge message': 'Bridge message', unknown: 'Unknown input' }[r.note] || 'No address'),
    r.note === 'peg reserve' ? el('span', { class: 'tag' }, 'peg reserve') : null,
    r.value ? el('span', { class: 'amount' }, `${tidy(r.value)} DOGE`) : el('span', { class: 'muted' }, 'amount unknown'))));
}

async function showTx(gen, txid) {
  loading(gen, 'Transaction');
  try {
    const t = await api(`/api/tx/${encodeURIComponent(txid)}`);
    const other = t.dogecoinTxid ? el('p', {}, 'On Dogecoin: ', dogeTx(t.dogecoinTxid)) : null;
    done(page(gen, 'Transaction',
      el('p', { class: `kind kind-${t.kind}` }, t.label),
      el('dl', { class: 'facts' },
        el('div', {}, el('dt', {}, 'ID'), el('dd', {}, el('code', {}, t.txid))),
        el('div', {}, el('dt', {}, 'Status'), el('dd', {}, t.confirmations > 0 ? `In a block, final (${t.confirmations} confirmation${t.confirmations === 1 ? '' : 's'})` : 'In the mempool')),
        t.time ? el('div', {}, el('dt', {}, 'Time'), el('dd', {}, new Date(t.time * 1000).toLocaleString())) : null,
        t.fee ? el('div', {}, el('dt', {}, 'Fee'), el('dd', { class: 'amount' }, `${tidy(t.fee)} DOGE`)) : null),
      other,
      el('div', { class: 'io-grid' },
        el('section', {}, el('h3', {}, 'From'), t.inputs.length ? ioTable(t.inputs) : el('p', { class: 'muted' }, 'Created by the block')),
        el('section', {}, el('h3', {}, 'To'), ioTable(t.outputs)))));
  } catch (err) {
    done(page(gen, 'Transaction', el('p', { class: 'result error' }, err.message)));
  }
}

async function showBlock(gen, id) {
  loading(gen, 'Block');
  try {
    const { block, transactions, txCount } = await api(`/api/block/${encodeURIComponent(id)}`);
    const more = txCount > transactions.length
      ? el('p', { class: 'muted' }, `Showing the first ${transactions.length} of ${txCount.toLocaleString('en-US')} transactions.`)
      : null;
    done(page(gen, `Block ${block.height.toLocaleString('en-US')}`,
      el('dl', { class: 'facts' },
        el('div', {}, el('dt', {}, 'Hash'), el('dd', {}, el('code', {}, block.hash))),
        el('div', {}, el('dt', {}, 'Time'), el('dd', {}, new Date(block.time * 1000).toLocaleString())),
        block.height > 0 ? el('div', {}, el('dt', {}, 'Previous'), el('dd', {}, vmBlock(block.height - 1))) : null),
      el('h3', {}, 'Transactions'),
      el('ol', { class: 'tx-list' }, ...transactions.map((t) =>
        el('li', {}, vmTx(t.txid), el('span', {}, t.label)))), more));
  } catch (err) {
    done(page(gen, 'Block', el('p', { class: 'result error' }, err.message)));
  }
}

async function showAddress(gen, addr) {
  loading(gen, 'Address');
  try {
    const a = await api(`/api/address/${encodeURIComponent(addr)}`);
    done(page(gen, 'Address',
      el('p', { class: 'address address-vm' }, a.address),
      el('p', { class: 'balance' }, el('span', { class: 'amount' }, tidy(a.confirmed)), ' ', el('span', { class: 'unit' }, 'DOGE')),
      el('h3', {}, 'Transactions'),
      el('ol', { class: 'tx-list' }, ...(a.history.length ? a.history.map((h) =>
        el('li', {}, vmTx(h.txid),
          el('span', { class: 'amount' }, `${h.net.startsWith('-') ? '−' : '+'}${tidy(h.net.replace('-', ''))} DOGE`),
          el('span', { class: 'move-time' }, h.confirmations > 0 ? '' : 'pending')))
        : [el('li', { class: 'empty' }, 'No transactions.')]))));
  } catch (err) {
    done(page(gen, 'Address', el('p', { class: 'result error' }, err.message)));
  }
}

// The explorer's front page lists activity, reserves and blocks; a
// transaction, block or address replaces it.
function route() {
  const gen = ++routeGen;
  const m = location.hash.match(/^#\/(tx|block|address)\/(.+)$/);
  const view = $('explorer-view');
  const front = [$('explorer-heading'), $('explorer-lists')];
  if (!m) {
    view.hidden = true;
    front.forEach((n) => { n.hidden = false; });
    document.title = 'DogecoinVM explorer';
    return;
  }
  view.hidden = false;
  front.forEach((n) => { n.hidden = true; });
  window.scrollTo(0, 0);
  let id;
  try {
    id = decodeURIComponent(m[2]);
  } catch {
    done(page(gen, 'Not found', el('p', { class: 'result error' }, 'That link is malformed.')));
    return;
  }
  ({ tx: showTx, block: showBlock, address: showAddress })[m[1]](gen, id);
}

// Search takes a txid or block hash (64 hex), a block height, or a
// DogecoinVM address.
async function search(q) {
  q = q.trim();
  if (/^[0-9a-f]{64}$/i.test(q)) {
    q = q.toLowerCase();
    // A 64-hex string is a transaction or a block hash. Only "not found" as
    // a transaction means try it as a block; any other failure shows on the
    // transaction page.
    const notTx = await api(`/api/tx/${q}`).then(() => false, (err) => err.status === 404);
    location.hash = notTx ? `#/block/${q}` : `#/tx/${q}`;
  } else if (/^\d+$/.test(q)) location.hash = `#/block/${q}`;
  else if (q) location.hash = `#/address/${encodeURIComponent(q)}`;
}

async function start() {
  try {
    info = await api('/api/info');
  } catch {
    info = {}; // Dogecoin links then show as plain text
  }
  $('search-form').addEventListener('submit', (e) => {
    e.preventDefault();
    search($('search').value);
    $('search').value = '';
  });
  window.addEventListener('hashchange', route);
  route();
  const refresh = () => { refreshActivity(); refreshReserves(); refreshBlocks(); };
  refresh();
  // New blocks on either chain refresh the lists straight away; the timer
  // is the fallback if the stream drops.
  const stream = new EventSource('/api/events');
  let pending = null;
  stream.addEventListener('block', () => {
    if (pending) return;
    pending = setTimeout(() => {
      pending = null;
      refresh();
    }, 100);
  });
  setInterval(() => { if (stream.readyState !== EventSource.OPEN) refresh(); }, 15000);
}

start();
