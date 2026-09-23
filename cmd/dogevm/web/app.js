import * as chain from './chain.js';
import { startExplorer } from './explorer.js';

const $ = (id) => document.getElementById(id);
const KEY_STORE = 'dogevm.key';
const WITHDRAW_STORE = 'dogevm.withdrawals';

let info = null;
let key = null; // Uint8Array, or null
let depositShownFor = null;
let utxos = [];

// generation counts key changes. Work started for one key checks it before
// touching the page, so a slow response never shows under another key.
let generation = 0;

// Stored values survive reloads; storage can be unavailable (private mode).
const store = {
  get(name) { try { return localStorage.getItem(name); } catch { return null; } },
  set(name, value) {
    try { localStorage.setItem(name, value); return localStorage.getItem(name) === value; } catch { return false; }
  },
  remove(name) { try { localStorage.removeItem(name); } catch { /* ignore */ } },
};

// APIError carries the HTTP status; status 0 means no response arrived.
class APIError extends Error {
  constructor(message, status) { super(message); this.status = status; }
}

async function api(path, body) {
  let res;
  try {
    res = await fetch(path, body === undefined ? {} : {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  } catch {
    throw new APIError("can't reach the bridge", 0);
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new APIError(data.error || `request failed (${res.status})`, res.status);
  return data;
}

// API amounts are DOGE decimal strings with 8 places; show them tidily.
const tidy = (s) => chain.formatDoge(chain.parseDoge(String(s).replace('-', '')));
const short = (txid) => `${txid.slice(0, 10)}…${txid.slice(-6)}`;

function showResult(el, message, ok) {
  el.textContent = message;
  el.className = 'result ' + (ok ? 'ok' : 'error');
}

function item(...texts) {
  const li = document.createElement('li');
  for (const t of texts) {
    const span = document.createElement('span');
    if (typeof t === 'string') span.textContent = t;
    else { span.textContent = t.text; span.className = t.class; }
    li.append(span);
  }
  return li;
}

function empty(text) {
  const li = document.createElement('li');
  li.className = 'empty';
  li.textContent = text;
  return li;
}

// --- tabs ------------------------------------------------------------------

const tabs = () => [...document.querySelectorAll('[role=tab]')].filter((t) => !t.hidden);

// selectTab shows a panel. Only the selected tab is in the tab order; arrow
// keys move between tabs, as the WAI-ARIA tabs pattern expects.
function selectTab(name) {
  for (const tab of document.querySelectorAll('[role=tab]')) {
    const selected = tab.id === `tab-${name}`;
    tab.setAttribute('aria-selected', String(selected));
    tab.tabIndex = selected ? 0 : -1;
    $(tab.getAttribute('aria-controls')).hidden = !selected;
  }
  if (name === 'deposit') showDeposit();
  if (name === 'withdraw') renderWithdrawals();
}

for (const tab of document.querySelectorAll('[role=tab]')) {
  tab.addEventListener('click', () => selectTab(tab.id.replace('tab-', '')));
}
selectTab('wallet');

document.querySelector('[role=tablist]').addEventListener('keydown', (e) => {
  const list = tabs();
  const i = list.indexOf(document.activeElement);
  if (i < 0) return;
  const next = {
    ArrowRight: list[(i + 1) % list.length],
    ArrowLeft: list[(i + list.length - 1) % list.length],
    Home: list[0],
    End: list[list.length - 1],
  }[e.key];
  if (!next) return;
  e.preventDefault();
  next.focus();
  selectTab(next.id.replace('tab-', ''));
});

// Theme: light unless the viewer picks dark or their system setting.
const THEME_STORE = 'dogevm.theme';
const themes = ['light', 'dark', 'system'];
function applyTheme(theme) {
  if (theme === 'light') document.documentElement.removeAttribute('data-theme');
  else document.documentElement.setAttribute('data-theme', theme);
  $('theme-toggle').textContent = `Theme: ${theme}`;
}
applyTheme(themes.includes(store.get(THEME_STORE)) ? store.get(THEME_STORE) : 'light');
$('theme-toggle').addEventListener('click', () => {
  const current = document.documentElement.getAttribute('data-theme') || 'light';
  const next = themes[(themes.indexOf(current) + 1) % themes.length];
  store.set(THEME_STORE, next);
  applyTheme(next);
});

document.addEventListener('click', async (e) => {
  const target = e.target.closest('button.copy');
  if (!target) return;
  const text = $(target.dataset.copy).textContent;
  try {
    await navigator.clipboard.writeText(text);
    target.textContent = 'Copied';
  } catch {
    // Select the text so it can be copied by hand.
    getSelection().selectAllChildren($(target.dataset.copy));
    target.textContent = 'Press Ctrl+C';
  }
  setTimeout(() => { target.textContent = 'Copy'; }, 2000);
});

// --- network and peg ---------------------------------------------------------

async function loadInfo() {
  info = await api('/api/info');
  $('confs-needed').textContent = info.depositConfirmations;
  $('vm-fee').textContent = tidy(info.vmFee);
  $('min-deposit').textContent = tidy(info.minDeposit);
  $('doge-fee').textContent = tidy(info.dogeFee);
  $('min-pegout').textContent = tidy(info.minPegOut);
  $('signers').textContent =
    `Held by ${info.signers.required} of ${info.signers.publicKeys.length} signers. Peg address on Dogecoin: ${info.pegAddress}`;
  const mainnet = info.dogecoinNetwork === 'mainnet';
  const band = $('network-band');
  band.hidden = false;
  $('import-key').placeholder = mainnet ? 'Q…, 6… or 64 hex characters' : 'c…, 9… or 64 hex characters';
  if (mainnet) {
    $('network-name').textContent = 'bridge beta';
    band.textContent = 'Beta, with real DOGE. Keep amounts small: the bridge is new and has not been audited.';
  } else {
    $('network-name').textContent = 'testnet bridge';
    band.textContent = 'Testnet. These coins have no value, and the network may be reset at any time.';
  }
  const limits = [];
  if (chain.parseDoge(info.maxDeposit) > 0n) limits.push(`Deposits over ${tidy(info.maxDeposit)} DOGE are not credited; they are held for a refund.`);
  if (chain.parseDoge(info.maxCirculating) > 0n) limits.push(`At most ${tidy(info.maxCirculating)} DOGE can be on DogecoinVM in total during the beta.`);
  $('deposit-limits').textContent = limits.length ? ' ' + limits.join(' ') : '';
  if (info.faucet.enabled) {
    $('tab-faucet').hidden = false;
    $('faucet-text').textContent =
      `The faucet sends ${tidy(info.faucet.amount)} DOGE straight to your DogecoinVM address, once a day.`;
  }
}

async function refreshStatus() {
  try {
    const s = await api('/api/status');
    $('vm-height').textContent = s.dogecoinvmHeight.toLocaleString('en-US');
    // While the bridge's Dogecoin node catches up, show how far it has got
    // rather than a block number that looks like the chain tip.
    const sync = s.dogecoinSync;
    const syncing = Boolean(sync && sync.syncing);
    $('sync-meter').hidden = !syncing;
    $('sync-of').hidden = !syncing;
    $('deposit-sync').hidden = !syncing;
    $('doge-height').textContent = s.dogecoinHeight.toLocaleString('en-US');
    if (syncing) {
      // Dogecoin Core's progress is weighted by transactions. Early blocks
      // are nearly empty, so a block count races ahead and then stalls.
      const pct = Math.min(99, Math.floor(sync.progress * 100));
      $('doge-height-label').textContent = 'Dogecoin node syncing';
      $('sync-fill').style.width = `${pct}%`;
      $('sync-of').textContent = `of ${sync.headers.toLocaleString('en-US')} (${pct}%)`;
      $('deposit-sync').textContent =
        `The bridge's Dogecoin node is catching up (${pct}%). It sees and credits new deposits only once it reaches the present.`;
    } else {
      $('doge-height-label').textContent = 'Dogecoin block';
    }
    if (!s.audit) {
      $('verdict').textContent = 'The bridge cannot read both chains right now.';
      $('verdict').className = 'peg-verdict bad';
      return;
    }
    const a = s.audit;
    const locked = chain.parseDoge(a.locked);
    const circulating = chain.parseDoge(a.circulating);
    const max = locked > circulating ? locked : circulating;
    const pct = (v) => (max === 0n ? 0 : Number((v * 1000n) / max) / 10);
    $('locked').textContent = tidy(a.locked);
    $('circulating').textContent = tidy(a.circulating);
    $('locked-fill').style.width = `${pct(locked)}%`;
    $('circulating-fill').style.width = `${pct(circulating)}%`;
    $('pending-in').textContent = tidy(a.pendingPegIns);
    $('pending-out').textContent = tidy(a.pendingPegOuts);
    let verdict;
    let cls = 'peg-verdict';
    if (!a.solvent) {
      verdict = 'Not fully backed: the bridge has stopped moving DOGE.';
      cls += ' bad';
    } else if (locked === 0n && circulating === 0n) {
      verdict = 'Nothing locked yet. The first deposit starts the peg.';
      cls += ' quiet';
    } else {
      verdict = 'Fully backed.';
    }
    // Announce the verdict only when it changes, not on every poll.
    if ($('verdict').textContent !== verdict) $('verdict').textContent = verdict;
    $('verdict').className = cls;
  } catch (err) {
    $('verdict').textContent = `Can't reach the bridge: ${err.message}`;
    $('verdict').className = 'peg-verdict bad';
  }
}

// --- key and wallet ------------------------------------------------------------

const myDest = () => chain.keyDestination(key);
const myAddress = () => chain.encodeAddress(myDest(), info.dogecoinvmVersions);

// setKey switches keys. It clears everything shown for the previous key.
function setKey(newKey) {
  key = newKey;
  generation++;
  utxos = [];
  depositShownFor = null;
  let warning = '';
  if (key) {
    if (!store.set(KEY_STORE, chain.hex(key))) {
      warning = "This browser won't keep your key: it will be gone when you close the page. Back it up now, under \"Show or remove your key\".";
    }
  } else {
    store.remove(KEY_STORE);
  }
  $('create-key').disabled = Boolean(key);
  $('import-submit').disabled = Boolean(key);
  $('key-warning').textContent = warning;
  $('key-warning').hidden = !warning;
  hideSecrets();
  $('key-details').open = false;
  $('balance').textContent = '…';
  $('balance-pending').textContent = '';
  $('history').replaceChildren();
  $('deposit-address').textContent = '…';
  $('deposit-verified').textContent = '';
  $('deposit-qr').replaceChildren();
  $('deposits').replaceChildren();
  $('withdrawals').replaceChildren();
  for (const id of ['send-result', 'withdraw-result', 'faucet-result']) {
    $(id).textContent = '';
    $(id).className = 'result';
  }
  renderKey();
}

function renderKey() {
  const has = key !== null;
  $('no-key').hidden = has;
  $('has-key').hidden = !has;
  for (const el of document.querySelectorAll('.needs-key')) el.hidden = has;
  for (const el of document.querySelectorAll('.with-key')) el.hidden = !has;
  if (!has) return;
  $('my-address').textContent = myAddress();
  refreshWallet();
  if (!$('panel-deposit').hidden) showDeposit();
  if (!$('panel-withdraw').hidden) renderWithdrawals();
}

// The key's text form is in the page only while "Show or remove your key"
// is open.
function hideSecrets() {
  $('wif-vm').textContent = '';
  $('wif-doge').textContent = '';
  $('secrets').hidden = true;
}
$('key-details').addEventListener('toggle', () => {
  if (!$('key-details').open || !key) { hideSecrets(); return; }
  $('wif-vm').textContent = chain.wif(key, info.dogecoinvmVersions);
  $('wif-doge').textContent = chain.wif(key, info.dogecoinVersions);
  $('secrets').hidden = false;
});

async function refreshWallet() {
  if (!key) return;
  const gen = generation;
  try {
    const a = await api(`/api/address/${myAddress()}`);
    if (gen !== generation) return;
    utxos = a.utxos;
    $('balance').textContent = tidy(a.confirmed);
    const pending = chain.parseDoge(a.pending);
    $('balance-pending').textContent = pending > 0n ? `${tidy(a.pending)} DOGE arriving in the next block` : '';
    $('history').replaceChildren(...(a.history.length === 0
      ? [empty('Nothing yet. Deposit DOGE from Dogecoin on the Deposit tab.')]
      : a.history.map((h) => {
        const sent = h.net.startsWith('-');
        return item(
          { text: short(h.txid), class: 'mono' },
          `${sent ? '−' : '+'}${tidy(h.net)} DOGE${h.confirmations > 0 ? '' : ' (pending)'}`,
        );
      })));
  } catch (err) {
    if (gen !== generation) return;
    $('balance').textContent = '…';
    $('balance-pending').textContent = `Can't load your balance: ${err.message}`;
  }
}

$('create-key').addEventListener('click', () => {
  // Never replace a key this browser already holds.
  if (key || store.get(KEY_STORE)) return;
  setKey(chain.newPrivateKey());
});

$('import-form').addEventListener('submit', (e) => {
  e.preventDefault();
  if (!info || key || store.get(KEY_STORE)) return;
  try {
    setKey(chain.parseKey($('import-key').value));
    $('import-key').value = '';
  } catch (err) {
    $('import-key').setCustomValidity(err.message);
    $('import-key').reportValidity();
    $('import-key').addEventListener('input', () => $('import-key').setCustomValidity(''), { once: true });
  }
});

$('forget-key').addEventListener('click', () => {
  if (confirm('Remove this key from the browser? Without a backup, its DOGE is gone.')) setKey(null);
});

const getRawTx = async (txid) => (await api(`/api/rawtx/${txid}`)).hex;

// pay signs a payment, calls beforeBroadcast with its txid (so a record
// exists even if the broadcast response is lost), and broadcasts it. It
// returns {txid, unknown}: unknown is true if the bridge never answered, so
// the payment may or may not have gone through.
async function pay(script, amount, data, beforeBroadcast) {
  const built = await chain.buildPayment({ key, utxos, getRawTx, script, amount, data });
  if (beforeBroadcast) beforeBroadcast(built.txid);
  try {
    const { txid } = await api('/api/tx', { hex: built.hex });
    if (txid !== built.txid) throw new Error(`the bridge reported txid ${txid}, expected ${built.txid}`);
  } catch (err) {
    if (err.status === 0 || err.status >= 500) return { txid: built.txid, unknown: true };
    err.rejected = true;
    throw err;
  } finally {
    setTimeout(refreshWallet, 1500);
  }
  return { txid: built.txid, unknown: false };
}

const unknownOutcome = (txid) =>
  `No answer from the bridge, so this may or may not have gone through. Check transaction ${short(txid)} in the explorer before trying again.`;

$('send-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const button = e.submitter;
  button.disabled = true;
  try {
    const to = chain.decodeAddress($('send-to').value, info.dogecoinvmVersions);
    const amount = chain.parseDoge($('send-amount').value);
    const { txid, unknown } = await pay(chain.pkScript(to), amount);
    if (unknown) showResult($('send-result'), unknownOutcome(txid), false);
    else showResult($('send-result'), `Sent. Transaction ${short(txid)}.`, true);
    $('send-form').reset();
  } catch (err) {
    showResult($('send-result'), err.message, false);
  } finally {
    button.disabled = false;
  }
});

// --- deposit -------------------------------------------------------------------

async function showDeposit() {
  if (!key) return;
  const gen = generation;
  const addr = myAddress();
  if (depositShownFor !== addr) {
    try {
      const d = await api('/api/deposit-address', { address: addr });
      if (gen !== generation) return;
      // The page derives the address itself from the signers' public keys;
      // if the server says otherwise, show nothing to send to.
      const expected = chain.depositAddress(myDest(), info.signers, info.dogecoinVersions);
      if (d.depositAddress !== expected) {
        $('deposit-address').textContent = 'Unavailable';
        $('deposit-qr').replaceChildren();
        showResult($('deposit-verified'),
          'The bridge sent a deposit address that does not match the peg signers, so it is not shown. Do not deposit until this is fixed.', false);
        return;
      }
      const { default: qrcode } = await import('./vendor/qrcode-generator-2.0.4/qrcode.mjs');
      if (gen !== generation) return;
      $('deposit-address').textContent = expected;
      $('deposit-verified').className = 'verified';
      $('deposit-verified').textContent =
        `Consistency check passed: your browser derived the same address from the ${info.signers.required}-of-${info.signers.publicKeys.length} peg signers' keys and your DogecoinVM address.`;
      const qr = qrcode(0, 'M');
      qr.addData(expected);
      qr.make();
      $('deposit-qr').innerHTML = qr.createSvgTag({ cellSize: 4, margin: 0, scalable: true });
      depositShownFor = addr;
    } catch (err) {
      if (gen !== generation) return;
      $('deposit-address').textContent = `Can't get a deposit address: ${err.message}`;
      return;
    }
  }
  refreshDeposits();
}

function depositStatus(d) {
  switch (d.status) {
    case 'credited': return { text: `Credited ${tidy(d.credited)} DOGE`, class: 'status-done' };
    case 'refunded': return { text: 'Refunded on Dogecoin', class: 'status-done' };
    case 'held': return { text: `Held for a refund: ${d.reason}`, class: 'status-held' };
    case 'waiting_for_capacity': return { text: 'Confirmed; waiting for room under the beta limit', class: 'status-waiting' };
    case 'crediting': return { text: 'Confirmed; crediting now', class: 'status-waiting' };
    default: return { text: `${Math.min(d.confirmations, d.required)} of ${d.required} confirmations`, class: 'status-waiting' };
  }
}

async function refreshDeposits() {
  if (!key || depositShownFor === null) return;
  const gen = generation;
  try {
    const deposits = await api(`/api/deposits/${myAddress()}`);
    if (gen !== generation) return;
    $('deposits').replaceChildren(...(deposits.length === 0
      ? [empty('No deposits yet. They show up here once Dogecoin sees them.')]
      : deposits.map((d) => item(`${tidy(d.amount)} DOGE`, depositStatus(d)))));
  } catch { /* try again on the next poll */ }
}

// --- withdraw ------------------------------------------------------------------

// Withdrawals are remembered per address, so switching keys shows the right
// ones.
const withdrawStore = () => `${WITHDRAW_STORE}.${myAddress()}`;
function savedWithdrawals() {
  try { return JSON.parse(store.get(withdrawStore()) || '[]'); } catch { return []; }
}
const saveWithdrawals = (list) => store.set(withdrawStore(), JSON.stringify(list.slice(0, 20)));

$('withdraw-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const button = e.submitter;
  button.disabled = true;
  let pendingTxid = null;
  try {
    const toText = $('withdraw-to').value.trim();
    const to = chain.decodeAddress(toText, info.dogecoinVersions);
    const amount = chain.parseDoge($('withdraw-amount').value);
    if (amount < chain.parseDoge(info.minPegOut)) throw new Error(`The minimum withdrawal is ${tidy(info.minPegOut)} DOGE.`);
    const reserve = chain.decodeAddress(info.reserveAddress, info.dogecoinvmVersions);
    const { txid, unknown } = await pay(chain.pkScript(reserve), amount, chain.pegOutData(to), (id) => {
      pendingTxid = id;
      saveWithdrawals([{ txid: id, to: toText, amount: chain.formatDoge(amount) }, ...savedWithdrawals()]);
    });
    if (unknown) showResult($('withdraw-result'), unknownOutcome(txid), false);
    else showResult($('withdraw-result'), 'Withdrawal sent. The bridge pays out once it is in a block.', true);
    $('withdraw-form').reset();
  } catch (err) {
    // The bridge refused it, so it will never be paid; forget it.
    if (err.rejected && pendingTxid) saveWithdrawals(savedWithdrawals().filter((w) => w.txid !== pendingTxid));
    showResult($('withdraw-result'), err.message, false);
  } finally {
    button.disabled = false;
    renderWithdrawals();
  }
});

async function renderWithdrawals() {
  if (!key) return;
  const gen = generation;
  const rows = savedWithdrawals().map((w) => {
    const li = item(`${w.amount} DOGE to ${w.to.slice(0, 8)}…`, { text: 'Checking…', class: 'status-waiting' });
    api(`/api/pegout/${w.txid}`).then((p) => {
      if (gen !== generation) return;
      if (p.status === 'paid') {
        li.lastChild.textContent = `Paid ${tidy(p.pays)} DOGE on Dogecoin`;
        li.lastChild.className = 'status-done';
      } else if (p.status === 'pending') {
        li.lastChild.textContent = 'Waiting for the bridge';
      } else {
        li.lastChild.textContent = 'Not in a block yet';
      }
    }).catch(() => { if (gen === generation) li.lastChild.textContent = 'Status unavailable'; });
    return li;
  });
  $('withdrawals').replaceChildren(...rows);
}

// --- faucet --------------------------------------------------------------------

$('faucet-claim').addEventListener('click', async (e) => {
  e.target.disabled = true;
  try {
    const r = await api('/api/faucet', { address: myAddress() });
    showResult($('faucet-result'), `Sent ${tidy(r.amount)} DOGE. It arrives in the next block.`, true);
    setTimeout(refreshWallet, 3000);
  } catch (err) {
    showResult($('faucet-result'), err.message, false);
  } finally {
    e.target.disabled = false;
  }
});

// --- start -----------------------------------------------------------------------

// start loads the network's settings, retrying until the bridge answers,
// then the saved key. The key buttons stay disabled until then, so a click
// during loading can't replace a saved key.
async function start() {
  for (let delay = 2000; ; delay = Math.min(delay * 2, 30000)) {
    try {
      await loadInfo();
      break;
    } catch (err) {
      $('wallet-start').textContent = `Can't reach the bridge (${err.message}). Retrying…`;
      $('wallet-start').className = 'result error';
      $('verdict').textContent = `Can't reach the bridge: ${err.message}`;
      $('verdict').className = 'peg-verdict bad';
      await new Promise((r) => { setTimeout(r, delay); });
    }
  }
  $('wallet-start').textContent = '';
  $('wallet-start').className = 'result';
  const saved = store.get(KEY_STORE);
  if (saved) {
    try {
      key = chain.parseKey(saved);
    } catch {
      $('wallet-start').textContent = 'The key saved in this browser is unreadable, so it was not loaded.';
      $('wallet-start').className = 'result error';
    }
  }
  if (!saved) {
    $('create-key').disabled = false;
    $('import-submit').disabled = false;
  }
  renderKey();
  refreshStatus();
  startExplorer(info);
  setInterval(() => {
    refreshStatus();
    refreshWallet();
    refreshDeposits();
    if (!$('panel-withdraw').hidden) renderWithdrawals();
  }, 15000);
}

start();
