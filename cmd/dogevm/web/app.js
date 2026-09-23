import * as chain from './chain.js';
import { startExplorer } from './explorer.js';
import * as passkey from './passkey.js';

const $ = (id) => document.getElementById(id);
const KEY_STORE = 'dogevm.key';
const PASSKEY_STORE = 'dogevm.key.passkey'; // the key, encrypted to a passkey
const WITHDRAW_STORE = 'dogevm.withdrawals';

let info = null;
let key = null; // Uint8Array, or null
let depositShownFor = null;
let utxos = []; // DogecoinVM
let dogeUtxos = []; // Dogecoin
let dogeState = 'unknown'; // unknown | ready | syncing | off

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

function showResult(el, message, ok, txid, network = 'vm') {
  el.textContent = message;
  el.className = 'result ' + (ok ? 'ok' : 'error');
  if (txid) {
    // The full transaction, in the right explorer, to check the outcome.
    const a = document.createElement('a');
    a.href = network === 'doge' ? `https://blockchair.com/dogecoin/transaction/${txid}` : `#/tx/${txid}`;
    if (network === 'doge') { a.target = '_blank'; a.rel = 'noopener noreferrer'; }
    a.textContent = txid;
    a.className = 'mono';
    el.append(' ', a);
  }
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
    target.textContent = /Mac|iPhone|iPad/.test(navigator.platform) ? 'Press ⌘C' : 'Press Ctrl+C';
  }
  setTimeout(() => { target.textContent = target.dataset.label || 'Copy'; }, 2000);
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
    // An emergency pause: nothing is credited or paid until it ends.
    const band = $('pause-band');
    band.hidden = !s.paused;
    if (s.paused) {
      const text = `The bridge is paused: ${s.paused.reason} Deposits and withdrawals already sent are processed when it resumes.`;
      if (band.textContent !== text) band.textContent = text;
    }
    $('vm-height').textContent = s.dogecoinvmHeight.toLocaleString('en-US');
    // While the bridge's Dogecoin node catches up, show how far it has got
    // rather than a block number that looks like the chain tip.
    const sync = s.dogecoinSync;
    const syncing = Boolean(sync && sync.syncing);
    $('sync-meter').hidden = !syncing;
    $('sync-of').hidden = !syncing;
    $('deposit-sync').hidden = !syncing;
    $('doge-height').textContent = s.dogecoinHeight.toLocaleString('en-US');
    if (sync && sync.available === false) {
      $('doge-height-label').textContent = 'Dogecoin node offline';
      $('doge-height').textContent = '–';
    } else if (syncing) {
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
    // Dogecoin's supply, from the bridge's own node, once it has caught up.
    const supply = s.dogecoinSupply;
    $('doge-supply').hidden = !supply;
    if (supply) {
      const total = BigInt(supply.amount);
      $('supply-total').textContent = total.toLocaleString('en-US');
      $('supply-total').title = `At Dogecoin block ${supply.height.toLocaleString('en-US')}`;
      const share = total > 0n ? Number(circulating) / Number(total * chain.KOINU) * 100 : 0;
      $('supply-share').textContent = share === 0 ? '0%'
        : share < 0.0001 ? 'under 0.0001%' : `${share.toPrecision(2)}%`;
    }
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
const myDogeAddress = () => chain.encodeAddress(myDest(), info.dogecoinVersions);

// setKey switches keys. It clears everything shown for the previous key.
// mode is 'store' (keep the key in this browser), 'unlocked' (a passkey
// opened it; keep nothing new) or 'lock' (forget it until unlocked again).
// setKey(null) with the default mode removes the key from the browser.
function setKey(newKey, mode = 'store') {
  key = newKey;
  generation++;
  utxos = [];
  dogeUtxos = [];
  dogeState = 'unknown';
  depositShownFor = null;
  let warning = '';
  if (key && mode === 'store') {
    if (!store.set(KEY_STORE, chain.hex(key))) {
      warning = "This browser won't keep your key: it will be gone when you close the page. Back it up now, under \"Show or remove your key\".";
    }
  } else if (!key && mode === 'store') {
    store.remove(KEY_STORE);
    store.remove(PASSKEY_STORE);
  }
  const held = Boolean(key) || store.get(PASSKEY_STORE) !== null;
  $('create-key').disabled = held;
  $('import-submit').disabled = held;
  $('passkey-result').textContent = '';
  $('key-warning').textContent = warning;
  $('key-warning').hidden = !warning;
  hideSecrets();
  $('key-details').open = false;
  $('balance').textContent = '…';
  $('balance-pending').textContent = '';
  $('history').replaceChildren();
  $('doge-balance').textContent = '…';
  $('doge-pending').textContent = '';
  $('doge-history').replaceChildren();
  $('doge-import').hidden = true;
  $('deposit-address').textContent = '…';
  $('deposit-verified').textContent = '';
  $('deposit-qr').replaceChildren();
  $('deposits').replaceChildren();
  $('withdrawals').replaceChildren();
  for (const id of ['send-result', 'withdraw-result', 'faucet-result', 'move-result', 'doge-import-result']) {
    $(id).textContent = '';
    $(id).className = 'result';
  }
  renderKey();
}

function renderKey() {
  const has = key !== null;
  const locked = !has && store.get(PASSKEY_STORE) !== null;
  $('wallet-locked').hidden = !locked;
  $('no-key').hidden = has || locked;
  $('has-key').hidden = !has;
  renderPasskey();
  for (const el of document.querySelectorAll('.needs-key')) el.hidden = has;
  for (const el of document.querySelectorAll('.with-key')) el.hidden = !has;
  if (!has) return;
  $('my-address').textContent = myAddress();
  // On mainnet the two networks share address versions, so one key has the
  // same address on both.
  const same = myAddress() === myDogeAddress();
  $('address-label').textContent = same ? 'Your address' : 'Your DogecoinVM address';
  $('address-note').textContent = same ? 'The same on Dogecoin and DogecoinVM: one key, two networks.' : '';
  $('doge-address-line').hidden = same;
  $('my-doge-address').textContent = same ? '' : myDogeAddress();
  refreshWallet();
  refreshDogeWallet();
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
    renderHistory($('history'), a.history, 'Nothing yet. Move DOGE over from Dogecoin on the Deposit tab.');
  } catch (err) {
    if (gen !== generation) return;
    $('balance').textContent = '…';
    $('balance-pending').textContent = `Can't load your balance: ${err.message}`;
  }
}

function renderHistory(list, history, emptyText) {
  list.replaceChildren(...(history.length === 0
    ? [empty(emptyText)]
    : history.map((h) => {
      const sent = h.net.startsWith('-');
      return item(
        { text: short(h.txid), class: 'mono' },
        `${sent ? '−' : '+'}${tidy(h.net)} DOGE${h.confirmations > 0 ? '' : ' (pending)'}`,
      );
    })));
}

// refreshDogeWallet shows the key's Dogecoin balance, registering the
// address with the bridge's Dogecoin index first if need be.
// The Dogecoin send option and the one-click move need the Dogecoin
// balance; until it's available they're disabled, with the reason shown.
function setDogeReady(ready, why = '') {
  const radio = document.querySelector('input[name=send-network][value=doge]');
  radio.disabled = !ready;
  if (!ready && radio.checked) document.querySelector('input[name=send-network][value=vm]').checked = true;
  radio.parentElement.title = ready ? '' : why;
  $('move-form').querySelector('button[type=submit]').disabled = !ready;
}

async function refreshDogeWallet() {
  if (!key || !info.dogeWallet) {
    $('doge-balance').textContent = '–';
    $('doge-pending').textContent = 'Not available from this bridge.';
    dogeState = 'off';
    setDogeReady(false, "This bridge doesn't serve Dogecoin balances.");
    return;
  }
  const gen = generation;
  const address = myDogeAddress();
  try {
    let a;
    try {
      a = await api(`/api/doge/address/${address}`);
    } catch (err) {
      if (err.status !== 404) throw err;
      await api('/api/doge/watch', { address });
      a = await api(`/api/doge/address/${address}`);
    }
    if (gen !== generation) return;
    dogeState = 'ready';
    setDogeReady(true);
    dogeUtxos = a.utxos;
    $('doge-balance').textContent = tidy(a.confirmed);
    const pending = chain.parseDoge(a.pending.replace('-', ''));
    $('doge-pending').textContent = pending === 0n ? ''
      : a.pending.startsWith('-') ? `${tidy(a.pending)} DOGE leaving, waiting for a block` : `${tidy(a.pending)} DOGE arriving, waiting for a block`;
    renderHistory($('doge-history'), a.history, 'Nothing yet. Send DOGE to your address from any Dogecoin wallet.');
    $('doge-import').hidden = false;
    $('move-available').textContent = `Available on Dogecoin: ${tidy(a.confirmed)} DOGE.`;
  } catch (err) {
    if (gen !== generation) return;
    dogeState = err.status === 503 ? 'syncing' : 'unknown';
    setDogeReady(false, "Available once the bridge's Dogecoin node has caught up.");
    $('doge-balance').textContent = '…';
    $('doge-pending').textContent = err.status === 503
      ? "Shows once the bridge's Dogecoin node has caught up."
      : `Can't load your Dogecoin balance: ${err.message}`;
    $('move-available').textContent = $('doge-pending').textContent;
    $('doge-history').replaceChildren(empty(err.status === 503
      ? "Your Dogecoin activity appears once the bridge's Dogecoin node has caught up."
      : "Can't load your Dogecoin activity right now."));
  }
}

$('doge-import-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const button = e.submitter;
  button.disabled = true;
  try {
    const txid = $('doge-import-txid').value.trim().toLowerCase();
    if (!/^[0-9a-f]{64}$/.test(txid)) throw new Error('A transaction ID is 64 hexadecimal characters.');
    const r = await api('/api/doge/import', { address: myDogeAddress(), txid });
    showResult($('doge-import-result'), `Added ${r.imported} payment${r.imported === 1 ? '' : 's'}.`, true);
    $('doge-import-form').reset();
    refreshDogeWallet();
  } catch (err) {
    showResult($('doge-import-result'), err.message, false);
  } finally {
    button.disabled = false;
  }
});

// --- passkey ---------------------------------------------------------------------

function renderPasskey() {
  const section = $('passkey-section');
  section.hidden = !key || !passkey.available();
  if (section.hidden) return;
  const backup = store.get(PASSKEY_STORE);
  $('passkey-status').textContent = backup
    ? 'Protected with a passkey: this browser keeps your key only in encrypted form. Keep the encrypted backup somewhere safe; with your passkey it restores this wallet on another device.'
    : 'Protect this wallet with a passkey. Your key is then kept encrypted, and opening the wallet takes Face ID, Touch ID or your security key.';
  $('passkey-protect').hidden = Boolean(backup);
  $('passkey-lock').hidden = !backup;
  $('passkey-backup-copy').hidden = !backup;
  $('passkey-backup').textContent = backup || '';
}

$('passkey-protect').addEventListener('click', async (e) => {
  e.target.disabled = true;
  // The passkey prompt takes a while; if the wallet changes meanwhile, this
  // backup belongs to the old key, so drop it.
  const gen = generation;
  const protecting = key;
  try {
    const backup = await passkey.protect(protecting);
    if (gen !== generation) return;
    if (!store.set(PASSKEY_STORE, backup)) throw new Error("This browser won't store the encrypted key, so nothing was changed.");
    store.remove(KEY_STORE);
    showResult($('passkey-result'), 'Done. Copy the encrypted backup and keep it somewhere safe.', true);
    renderPasskey();
  } catch (err) {
    showResult($('passkey-result'), err.message, false);
  } finally {
    e.target.disabled = false;
  }
});

$('passkey-lock').addEventListener('click', () => setKey(null, 'lock'));

$('unlock-key').addEventListener('click', async (e) => {
  e.target.disabled = true;
  try {
    setKey(await passkey.unlock(store.get(PASSKEY_STORE)), 'unlocked');
  } catch (err) {
    showResult($('unlock-result'), err.message, false);
  } finally {
    e.target.disabled = false;
  }
});

$('create-key').addEventListener('click', () => {
  // Never replace a key this browser already holds.
  if (key || store.get(KEY_STORE) || store.get(PASSKEY_STORE)) return;
  setKey(chain.newPrivateKey());
});

$('import-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  if (!info || key || store.get(KEY_STORE) || store.get(PASSKEY_STORE)) return;
  const value = $('import-key').value;
  try {
    if (passkey.isBackup(value)) {
      // A passkey backup: open it with its passkey, and keep it encrypted.
      const restored = await passkey.unlock(value);
      if (!store.set(PASSKEY_STORE, value.trim())) throw new Error("This browser won't store the encrypted key.");
      setKey(restored, 'unlocked');
    } else {
      setKey(chain.parseKey(value));
    }
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

// Each network's coins, and where to check and broadcast its transactions.
const networks = {
  vm: {
    utxos: () => utxos,
    getRawTx: async (txid) => (await api(`/api/rawtx/${txid}`)).hex,
    broadcast: '/api/tx',
    refresh: () => refreshWallet(),
  },
  doge: {
    utxos: () => dogeUtxos,
    getRawTx: async (txid) => (await api(`/api/doge/rawtx/${txid}`)).hex,
    broadcast: '/api/doge/tx',
    refresh: () => refreshDogeWallet(),
  },
};

// pay signs a payment on a network ('vm' or 'doge'), calls beforeBroadcast
// with its txid (so a record exists even if the broadcast response is lost),
// and broadcasts it. It returns {txid, unknown}: unknown is true if the
// bridge never answered, so the payment may or may not have gone through.
async function pay(script, amount, data, beforeBroadcast, network = 'vm') {
  const net = networks[network];
  if (network === 'doge' && dogeState !== 'ready') {
    throw new Error("Your Dogecoin balance isn't available yet; try again once it shows.");
  }
  const built = await chain.buildPayment({ key, utxos: net.utxos(), getRawTx: net.getRawTx, script, amount, data });
  if (beforeBroadcast) beforeBroadcast(built.txid);
  try {
    const { txid } = await api(net.broadcast, { hex: built.hex });
    if (txid !== built.txid) throw new Error(`the bridge reported txid ${txid}, expected ${built.txid}`);
  } catch (err) {
    if (err.status === 0 || err.status >= 500) return { txid: built.txid, unknown: true };
    err.rejected = true;
    throw err;
  } finally {
    setTimeout(net.refresh, 1500);
  }
  return { txid: built.txid, unknown: false };
}

const unknownOutcome = 'No answer from the bridge, so this may or may not have gone through. Check the transaction before trying again:';

$('send-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const button = e.submitter;
  button.disabled = true;
  try {
    const network = document.querySelector('input[name=send-network]:checked').value;
    const versions = network === 'doge' ? info.dogecoinVersions : info.dogecoinvmVersions;
    const to = chain.decodeAddress($('send-to').value, versions);
    const amount = chain.parseDoge($('send-amount').value);
    const { txid, unknown } = await pay(chain.pkScript(to), amount, undefined, undefined, network);
    const where = network === 'doge' ? 'on Dogecoin' : 'on DogecoinVM';
    if (unknown) showResult($('send-result'), unknownOutcome, false, txid, network);
    else showResult($('send-result'), `Sent ${where}. Transaction:`, true, txid, network);
    $('send-to').value = '';
    $('send-amount').value = '';
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

// The one-click deposit: pay the personal deposit address from the key's own
// Dogecoin balance. The address is the one this page derives from the
// signers' keys, and it is registered with the bridge first.
$('move-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const button = e.submitter;
  button.disabled = true;
  try {
    const amount = chain.parseDoge($('move-amount').value);
    const min = chain.parseDoge(info.minDeposit);
    const max = chain.parseDoge(info.maxDeposit);
    if (amount < min) throw new Error(`The smallest deposit is ${tidy(info.minDeposit)} DOGE.`);
    if (max > 0n && amount > max) {
      throw new Error(`During the beta a deposit can be at most ${tidy(info.maxDeposit)} DOGE; a larger one is held for a refund.`);
    }
    if (depositShownFor !== myAddress()) await showDeposit();
    if (depositShownFor !== myAddress()) throw new Error("Your deposit address couldn't be checked, so nothing was sent. See below.");
    const expected = chain.depositAddress(myDest(), info.signers, info.dogecoinVersions);
    const script = chain.pkScript(chain.decodeAddress(expected, info.dogecoinVersions));
    const { txid, unknown } = await pay(script, amount, undefined, undefined, 'doge');
    if (unknown) {
      showResult($('move-result'), unknownOutcome, false, txid, 'doge');
    } else {
      showResult($('move-result'),
        `Sent to your deposit address. It's credited on DogecoinVM after ${info.depositConfirmations} Dogecoin confirmations, about ${info.depositConfirmations} minutes. Transaction:`, true, txid, 'doge');
    }
    $('move-amount').value = '';
    setTimeout(refreshDeposits, 3000);
  } catch (err) {
    showResult($('move-result'), err.message, false);
  } finally {
    button.disabled = false;
  }
});

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
    if (unknown) showResult($('withdraw-result'), unknownOutcome, false, txid);
    else showResult($('withdraw-result'), 'Withdrawal sent. The bridge pays out once it is in a block. Transaction:', true, txid);
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

$('withdraw-to-mine').addEventListener('click', () => {
  if (!key) return;
  $('withdraw-to').value = myDogeAddress();
  $('withdraw-amount').focus();
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
  if (!saved && store.get(PASSKEY_STORE) === null) {
    $('create-key').disabled = false;
    $('import-submit').disabled = false;
  }
  renderKey();
  refreshStatus();
  startExplorer(info);
  setInterval(() => {
    refreshStatus();
    refreshWallet();
    refreshDogeWallet();
    refreshDeposits();
    if (!$('panel-withdraw').hidden) renderWithdrawals();
  }, 15000);
}

start();
