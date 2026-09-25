import * as chain from './chain.js';
import * as passkey from './passkey.js';
import { confirmationsFor, describeTiers } from './tiers.js';

const $ = (id) => document.getElementById(id);
const KEY_STORE = 'dogevm.key';
const PASSKEY_STORE = 'dogevm.key.passkey'; // the key, encrypted to a passkey
const WITHDRAW_STORE = 'dogevm.withdrawals';
const SETUP_STORE = 'dogevm.setup'; // per address: {backedUp, hidden}

let info = null;
let key = null; // Uint8Array, or null
let depositShownFor = null;
let utxos = []; // DogecoinVM
let dogeUtxos = []; // Dogecoin
let dogeState = 'unknown'; // unknown | ready | syncing | off
// What the setup checklist knows of each network: whether the key has ever
// had DOGE there. null until the balance has loaded.
let seenVm = null;
let seenDoge = null;
// Confirmed balances as the API gives them, or null until loaded.
let vmBalance = null;
let dogeBalance = null;

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
    a.href = network === 'doge' ? `https://blockchair.com/dogecoin/transaction/${txid}` : `/explorer#/tx/${txid}`;
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
  $('confs-needed-top').textContent = describeTiers(info);
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
    $('network-name').textContent = 'beta';
    band.textContent = 'Beta, with real DOGE. Keep amounts small: the bridge is new and has not been audited.';
  } else {
    $('network-name').textContent = 'testnet';
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
    dogeBlockTime = s.dogecoinBlockTime || 0;
    renderInflight(); // the quiet-Dogecoin note counts up between blocks
    $('vm-height').textContent = s.dogecoinvmHeight.toLocaleString('en-US');
    // Finality, measured live on this site's own payments.
    const f = s.finality;
    $('vm-finality').hidden = !f;
    if (f) {
      const secs = (ms) => `${(ms / 1000).toFixed(1)} s`;
      $('vm-finality').textContent = f.payments === 1
        ? `Last payment final in ${secs(f.medianMs)}.`
        : `Payments final in ${secs(f.medianMs)}: the median of the last ${f.payments}.`;
    }
    // While the bridge's Dogecoin node catches up, show how far it has got
    // rather than a block number that looks like the chain tip.
    const sync = s.dogecoinSync;
    const syncing = Boolean(sync && sync.syncing);
    const offline = Boolean(sync && sync.available === false);
    $('sync-meter').hidden = !syncing || offline;
    $('sync-of').hidden = !syncing && !offline;
    $('deposit-sync').hidden = !syncing;
    $('doge-height').textContent = s.dogecoinHeight.toLocaleString('en-US');
    if (offline) {
      $('doge-height').textContent = '–';
      $('sync-of').textContent = "The bridge's Dogecoin node is offline.";
    } else if (syncing) {
      // Dogecoin Core's progress is weighted by transactions. Early blocks
      // are nearly empty, so a block count races ahead and then stalls.
      const pct = Math.min(99, Math.floor(sync.progress * 100));
      $('sync-fill').style.width = `${pct}%`;
      $('sync-of').textContent = `Node catching up: ${pct}%. Latest block ${sync.headers.toLocaleString('en-US')}.`;
      $('deposit-sync').textContent =
        `The bridge's Dogecoin node is catching up (${pct}%). It sees and credits new deposits only once it reaches the present.`;
    }
    if (!s.audit) {
      $('verdict').textContent = 'The bridge cannot read both chains right now.';
      $('verdict').className = 'peg-verdict bad';
      return;
    }
    const a = s.audit;
    const locked = chain.parseDoge(a.locked);
    const circulating = chain.parseDoge(a.circulating);
    // During the beta the bars are drawn against the circulating cap, so
    // they show how much of the beta's room is used.
    const cap = chain.parseDoge(info.maxCirculating);
    let max = locked > circulating ? locked : circulating;
    if (cap > max) max = cap;
    $('peg-capacity').textContent = cap > 0n
      ? `Beta capacity: ${tidy(a.circulating)} of ${tidy(info.maxCirculating)} DOGE in use.`
      : '';
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
  seenVm = null;
  seenDoge = null;
  lastDeposits = [];
  withdrawalStatus.clear();
  shownConfirmations.clear();
  $('inflight').hidden = true;
  vmBalance = null;
  dogeBalance = null;
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
  renderAvailable();
  renderKey();
}

function renderKey() {
  const has = key !== null;
  const locked = !has && store.get(PASSKEY_STORE) !== null;
  $('wallet-locked').hidden = !locked;
  $('no-key').hidden = has || locked;
  $('has-key').hidden = !has;
  renderPasskey();
  renderSetup();
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
  refreshDeposits();
  refreshWithdrawalStatus();
  renderInflight();
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
    settleOutgoing('vm', a.history);
    vmBalance = a.confirmed;
    renderAvailable();
    seenVm = a.history.length > 0 || chain.parseDoge(a.confirmed) > 0n;
    renderSetup();
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

// renderAvailable shows what can be spent next to the Send and Withdraw
// forms, for the network chosen.
function renderAvailable() {
  const onDoge = document.querySelector('input[name=send-network]:checked').value === 'doge';
  // box shows a network's spendable balance, or why it isn't shown yet.
  const box = (el, network, balance, note) => {
    const label = document.createElement('span');
    label.className = 'available-label';
    label.textContent = `Available on ${network}`;
    const value = document.createElement('span');
    if (balance === null) {
      value.className = 'available-note';
      value.textContent = note;
    } else {
      value.className = 'available-amount amount';
      value.textContent = `${tidy(balance)} DOGE`;
    }
    el.replaceChildren(label, value);
  };
  const send = $('send-available');
  send.className = `available ${onDoge ? 'available-doge' : 'available-vm'}`;
  if (onDoge) box(send, 'Dogecoin', dogeBalance, $('doge-pending').textContent || 'Loading…');
  else box(send, 'DogecoinVM', vmBalance, 'Loading…');
  box($('withdraw-available'), 'DogecoinVM', vmBalance, 'Loading…');
}
for (const radio of document.querySelectorAll('input[name=send-network]')) radio.addEventListener('change', renderAvailable);

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
    settleOutgoing('doge', a.history);
    const change = outgoing().filter((o) => o.network === 'doge').reduce((n, o) => n + BigInt(o.change), 0n);
    $('doge-pending').textContent = pending === 0n ? ''
      : a.pending.startsWith('-')
        ? `${tidy(a.pending)} DOGE leaving${change > 0n ? `; ${chain.formatDoge(change)} DOGE change comes back` : ''} when it confirms, usually within a minute.`
        : `${tidy(a.pending)} DOGE arriving, waiting for a block.`;
    renderHistory($('doge-history'), a.history, 'Nothing yet. Send DOGE to your address from any Dogecoin wallet.');
    dogeBalance = a.confirmed;
    renderAvailable();
    seenDoge = a.history.length > 0 || chain.parseDoge(a.confirmed) > 0n;
    renderSetup();
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
    renderAvailable();
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

// --- setup checklist -------------------------------------------------------------

// The checklist walks a new wallet through backing up, protecting, funding
// and a first round trip. Each step ticks itself off from what the wallet
// can see, and points at the controls that already do the work.
const setupStore = () => `${SETUP_STORE}.${myAddress()}`;
function setupState() {
  try { return JSON.parse(store.get(setupStore()) || '{}'); } catch { return {}; }
}
function saveSetup(change) {
  if (!key) return;
  store.set(setupStore(), JSON.stringify({ ...setupState(), ...change }));
  renderSetup();
}

// goTo shows a control, opening its tab or section first, and focuses it.
function goTo(tab, id) {
  if (tab) selectTab(tab);
  const el = $(id);
  el.scrollIntoView({ block: 'center', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
  el.focus({ preventScroll: true });
}

function setupSteps() {
  const state = setupState();
  const protectedByPasskey = store.get(PASSKEY_STORE) !== null;
  const funded = seenDoge === true || seenVm === true;
  const steps = [
    {
      title: 'Back up your key',
      text: "It's your wallet on both networks. Without a copy, DOGE here can't be recovered if this browser's data is cleared.",
      done: state.backedUp === true,
      actions: [
        ['Show my key', () => { $('key-details').open = true; goTo(null, 'key-details'); }],
        ["I've saved it", () => saveSetup({ backedUp: true })],
      ],
    },
    passkey.available() && {
      title: 'Protect it with a passkey',
      text: 'Optional. Your key is then kept encrypted, and opening the wallet takes your passkey, for example in 1Password.',
      done: protectedByPasskey,
      actions: [['Protect with a passkey', () => $('passkey-protect').click()]],
    },
    {
      title: 'Get DOGE on Dogecoin',
      text: dogeState === 'syncing'
        ? "Send DOGE to your address above from any wallet or exchange. It shows here once the bridge's Dogecoin node has caught up."
        : 'Send DOGE to your address above from any wallet or exchange.',
      done: funded,
      actions: [['Copy my address', async (button) => {
        try {
          await navigator.clipboard.writeText(myDogeAddress());
          button.textContent = 'Copied';
          setTimeout(() => { button.textContent = 'Copy my address'; }, 2000);
        } catch {
          goTo(null, 'my-address');
          getSelection().selectAllChildren($('my-address'));
        }
      }]],
    },
    {
      title: 'Move it to DogecoinVM',
      text: 'Move DOGE across the bridge from the Deposit tab. Payments on DogecoinVM are final in about two seconds.',
      done: seenVm === true,
      actions: [['Go to Deposit', () => goTo('deposit', 'move-amount')]],
    },
    {
      title: 'Withdraw some back',
      text: 'Send DOGE back to your own Dogecoin address to complete the round trip.',
      done: savedWithdrawals().length > 0,
      actions: [['Go to Withdraw', () => goTo('withdraw', 'withdraw-to')]],
    },
  ];
  return steps.filter(Boolean);
}

function renderSetup() {
  if (!key || !info) {
    $('setup').hidden = true;
    $('setup-reopen').hidden = true;
    return;
  }
  const steps = setupSteps();
  const finished = steps.every((s) => s.done);
  const hidden = setupState().hidden === true || finished;
  $('setup').hidden = hidden;
  $('setup-reopen').hidden = !hidden || finished;
  if (hidden) return;
  const next = steps.findIndex((s) => !s.done);
  $('setup-steps').replaceChildren(...steps.map((s, i) => {
    const li = document.createElement('li');
    li.className = s.done ? 'done' : i === next ? 'current' : '';
    const title = document.createElement('p');
    title.className = 'setup-title';
    title.textContent = s.title;
    if (s.done) title.append(Object.assign(document.createElement('span'), { className: 'visually-hidden', textContent: ' (done)' }));
    li.append(title);
    if (!s.done) {
      const text = document.createElement('p');
      text.className = 'setup-text';
      text.textContent = s.text;
      li.append(text);
      if (i === next) {
        const row = document.createElement('div');
        row.className = 'setup-actions';
        s.actions.forEach(([label, run], n) => {
          const b = document.createElement('button');
          b.type = 'button';
          if (n === 0) b.className = 'primary';
          b.textContent = label;
          b.addEventListener('click', () => run(b));
          row.append(b);
        });
        li.append(row);
      }
    }
    return li;
  }));
}

$('setup-hide').addEventListener('click', () => { saveSetup({ hidden: true }); $('setup-show').focus(); });
$('setup-show').addEventListener('click', () => { saveSetup({ hidden: false }); $('setup-title').focus(); });

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
    renderSetup();
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
    // An imported key is one the viewer already has a copy of.
    saveSetup({ backedUp: true });
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

// --- review before signing ----------------------------------------------------

function reviewLine(label, address, value, note, cls) {
  const li = document.createElement('li');
  if (cls) li.className = cls;
  const add = (tag, className, text) => {
    const n = document.createElement(tag);
    n.className = className;
    n.textContent = text;
    li.append(n);
  };
  add('span', 'review-label', label);
  add('span', 'review-amount', value === null ? '' : `${chain.formatDoge(value)} DOGE`);
  if (address) add('span', 'review-address', address);
  if (note) add('p', 'review-note', note);
  return li;
}

// review shows what a planned transaction does, decoded from its own bytes
// rather than from the form, and resolves true only if the viewer confirms.
// context names addresses the page expects: the peg reserve for a
// withdrawal, the deposit address for a move.
function review(plan, network, context = {}) {
  const doge = network === 'doge';
  const versions = doge ? info.dogecoinVersions : info.dogecoinvmVersions;
  const mine = doge ? myDogeAddress() : myAddress();
  const lines = [];
  const problems = [];
  let change = 0n;
  let outTotal = 0n;
  for (const o of chain.reviewOutputs(plan.unsignedHex, versions)) {
    outTotal += o.value;
    if (o.address === mine) {
      change += o.value;
      lines.push(reviewLine('Back to you (change)', o.address, o.value));
    } else if (o.data) {
      const dest = chain.pegOutDestination(o.data, info.dogecoinVersions);
      if (dest && context.withdrawTo === dest) {
        lines.push(reviewLine('The bridge then pays on Dogecoin', dest, null));
      } else {
        problems.push('The transaction carries a bridge instruction this page did not ask for.');
      }
    } else if (o.address && o.address === context.reserve) {
      const gets = o.value - chain.parseDoge(info.dogeFee);
      lines.push(reviewLine('To the bridge, to withdraw', o.address, o.value,
        `You receive ${chain.formatDoge(gets > 0n ? gets : 0n)} DOGE on Dogecoin, after the ${tidy(info.dogeFee)} DOGE Dogecoin fee.`));
    } else if (o.address && o.address === context.deposit) {
      const gets = o.value - chain.parseDoge(info.vmFee);
      lines.push(reviewLine('To your deposit address', o.address, o.value,
        `Credited as ${chain.formatDoge(gets > 0n ? gets : 0n)} DOGE on DogecoinVM after ${confirmationsFor(info, o.value)} Dogecoin confirmation${confirmationsFor(info, o.value) === 1 ? '' : 's'}, less the ${tidy(info.vmFee)} DOGE bridge fee.`));
    } else if (o.address) {
      lines.push(reviewLine('To', o.address, o.value));
    } else {
      problems.push('The transaction pays a script this page cannot read.');
    }
  }
  if (outTotal + plan.fee !== plan.inputTotal) problems.push("The transaction's amounts don't add up.");
  if (context.reserve && context.withdrawTo === undefined) problems.push('A withdrawal needs a Dogecoin address.');
  lines.push(reviewLine('Network fee', null, plan.fee));
  lines.push(reviewLine('Leaves your wallet', null, plan.inputTotal - change, null, 'review-total'));

  $('review-network').textContent = doge ? 'On Dogecoin' : 'On DogecoinVM';
  $('review-network').className = `review-network${doge ? ' doge' : ''}`;
  $('review-lines').replaceChildren(...lines);
  $('review-problem').hidden = problems.length === 0;
  $('review-problem').textContent = problems.length ? `${problems.join(' ')} Don't send it.` : '';
  $('review-confirm').disabled = problems.length > 0;

  const dialog = $('review');
  return new Promise((resolve) => {
    dialog.returnValue = '';
    dialog.addEventListener('close', () => resolve(problems.length === 0 && dialog.returnValue === 'confirm'), { once: true });
    dialog.showModal();
    $('review-cancel').focus();
  });
}

// cancelled is the error a payment ends with when its review is dismissed.
const cancelled = () => Object.assign(new Error('Cancelled. Nothing was sent.'), { cancelled: true });

// showFailure shows why a payment didn't happen; a cancel isn't an error.
function showFailure(el, err) {
  if (!err.cancelled) return showResult(el, err.message, false);
  el.textContent = err.message;
  el.className = 'result';
}

// pay plans a payment on a network ('vm' or 'doge'), shows it for review,
// signs it once confirmed, calls beforeBroadcast with its txid (so a record
// exists even if the broadcast response is lost), and broadcasts it. It
// returns {txid, unknown}: unknown is true if the bridge never answered, so
// the payment may or may not have gone through.
async function pay(script, amount, data, beforeBroadcast, network = 'vm', context = {}) {
  const net = networks[network];
  if (network === 'doge' && dogeState !== 'ready') {
    throw new Error("Your Dogecoin balance isn't available yet; try again once it shows.");
  }
  const gen = generation;
  // Coins a payment still confirming already spends can't be spent again.
  const inUse = coinsInUse(network);
  const plan = await chain.planPayment({
    key, utxos: net.utxos().filter((u) => !inUse.has(`${u.txid}:${u.vout}`)), getRawTx: net.getRawTx,
    script, amount, data, feePerByte: network === 'vm' ? chain.VM_FEE_PER_BYTE : undefined,
  });
  if (!(await review(plan, network, context))) throw cancelled();
  if (gen !== generation || !key) throw new Error('The wallet changed during review, so nothing was sent.');
  const built = await chain.signPlan(plan, key);
  if (beforeBroadcast) beforeBroadcast(built.txid);
  rememberOutgoing(plan, built.txid, network, context, amount);
  try {
    const { txid } = await api(net.broadcast, { hex: built.hex });
    if (txid !== built.txid) throw new Error(`the bridge reported txid ${txid}, expected ${built.txid}`);
  } catch (err) {
    if (err.status === 0 || err.status >= 500) return { txid: built.txid, unknown: true };
    err.rejected = true;
    forgetOutgoing(built.txid);
    throw err;
  } finally {
    renderInflight();
    setTimeout(net.refresh, 1500);
  }
  return { txid: built.txid, unknown: false };
}


// --- payments in flight ------------------------------------------------------------

// Each payment this wallet sends is remembered until it confirms: the coins
// it spends (so they aren't offered again) and its change (so a balance
// that reads 0 while it confirms shows where the DOGE is).
const OUTGOING_STORE = 'dogevm.outgoing';
const outgoingStore = () => `${OUTGOING_STORE}.${myAddress()}`;
function outgoing() {
  try { return JSON.parse(store.get(outgoingStore()) || '[]'); } catch { return []; }
}
const saveOutgoing = (list) => store.set(outgoingStore(), JSON.stringify(list.slice(0, 50)));

function rememberOutgoing(plan, txid, network, context, amount) {
  const versions = network === 'doge' ? info.dogecoinVersions : info.dogecoinvmVersions;
  const mine = network === 'doge' ? myDogeAddress() : myAddress();
  const outs = chain.reviewOutputs(plan.unsignedHex, versions);
  const change = outs.filter((o) => o.address === mine).reduce((n, o) => n + o.value, 0n);
  const kind = context.deposit ? 'move' : context.reserve ? 'withdraw' : 'send';
  const to = context.withdrawTo || context.deposit || outs.find((o) => o.address && o.address !== mine)?.address || '';
  saveOutgoing([{
    txid, network, kind, to, amount: String(amount), change: String(change), time: Date.now(),
    spent: plan.tx.inputs.map((i) => `${i.txid}:${i.vout}`),
  }, ...outgoing().filter((o) => o.txid !== txid)]);
}

function forgetOutgoing(txid) {
  saveOutgoing(outgoing().filter((o) => o.txid !== txid));
}

function coinsInUse(network) {
  return new Set(outgoing().filter((o) => o.network === network).flatMap((o) => o.spent));
}

// settleOutgoing drops payments a chain now shows confirmed, or that it has
// never shown after an hour (dropped by the network).
function settleOutgoing(network, history) {
  const seen = new Map(history.map((h) => [h.txid, h.confirmations]));
  const hour = 60 * 60 * 1000;
  const keep = outgoing().filter((o) => o.network !== network
    || (seen.has(o.txid) ? seen.get(o.txid) === 0 : Date.now() - o.time < hour));
  if (keep.length !== outgoing().length) saveOutgoing(keep);
}

// What the panel draws from, refreshed as each list loads.
let lastDeposits = [];
let dogeBlockTime = 0; // when the latest Dogecoin block was found, unix seconds
const withdrawalStatus = new Map(); // txid -> /api/pegout answer
const shownConfirmations = new Map(); // deposit -> confirmations drawn, to animate new ones

async function refreshWithdrawalStatus() {
  if (!key) return;
  const gen = generation;
  const open = savedWithdrawals().filter((w) => !(withdrawalStatus.get(w.txid)?.paymentConfirmations > 0));
  await Promise.all(open.map(async (w) => {
    try {
      const p = await api(`/api/pegout/${w.txid}`);
      if (gen === generation) withdrawalStatus.set(w.txid, p);
    } catch { /* next block */ }
  }));
  if (gen === generation) renderInflight();
}

const minutes = (n) => (n <= 1 ? 'about a minute' : `about ${n} minutes`);

// card builds one in-flight entry: a title, an optional progress row, and
// a line of detail.
function card(title, progress, detail, cls = '') {
  const li = document.createElement('li');
  li.className = `inflight-card ${cls}`;
  const h = document.createElement('p');
  h.className = 'inflight-title';
  h.textContent = title;
  li.append(h);
  if (progress) li.append(progress);
  const d = document.createElement('p');
  d.className = 'inflight-detail';
  d.textContent = detail;
  li.append(d);
  return li;
}

// blocks draws confirmations as a row of blocks, the newest popping in.
function blocks(id, have, need) {
  const row = document.createElement('div');
  row.className = 'inflight-blocks';
  row.setAttribute('role', 'img');
  row.setAttribute('aria-label', `${Math.min(have, need)} of ${need} confirmations`);
  const before = shownConfirmations.get(id) ?? have;
  for (let i = 0; i < need; i++) {
    const b = document.createElement('span');
    b.className = i < have ? 'block filled' : 'block';
    if (i < have && i >= before) b.classList.add('new');
    row.append(b);
  }
  shownConfirmations.set(id, have);
  return row;
}

// dogeWaiting marks a card as waiting on the next Dogecoin block.
function dogeWaiting(c) {
  c.dataset.dogecoin = '1';
  return c;
}

// steps draws a withdrawal's stages.
function steps(list) {
  const ol = document.createElement('ol');
  ol.className = 'inflight-steps';
  for (const [label, done] of list) {
    const li = document.createElement('li');
    li.className = done ? 'done' : '';
    li.textContent = label;
    ol.append(li);
  }
  return ol;
}

function renderInflight() {
  if (!key || !info) return;
  const cards = [];
  const depositTxids = new Set(lastDeposits.map((d) => d.txid));

  for (const d of lastDeposits) {
    if (['credited', 'refunded'].includes(d.status)) continue;
    const gets = chain.formatDoge(chain.parseDoge(d.amount) - chain.parseDoge(info.vmFee));
    const title = `Moving ${tidy(d.amount)} DOGE to DogecoinVM`;
    if (d.status === 'held') {
      cards.push(card(title, null, `Held for a refund: ${d.reason}`, 'held'));
    } else if (d.status === 'crediting' || d.confirmations >= d.required) {
      cards.push(card(title, blocks(`${d.txid}:${d.vout}`, d.required, d.required), `Confirmed. Crediting ${gets} DOGE now.`));
    } else if (d.status === 'waiting_for_capacity') {
      cards.push(card(title, null, 'Confirmed, and waiting for room under the beta limit.'));
    } else {
      const left = d.required - d.confirmations;
      cards.push(dogeWaiting(card(title, blocks(`${d.txid}:${d.vout}`, d.confirmations, d.required),
        `${d.confirmations} of ${d.required} confirmations. ${minutes(left)[0].toUpperCase()}${minutes(left).slice(1)} left, then ${gets} DOGE arrives.`)));
    }
  }

  for (const w of savedWithdrawals()) {
    const p = withdrawalStatus.get(w.txid);
    if (!p || p.paymentConfirmations > 0) continue;
    const final = p.status === 'pending' || p.status === 'paid';
    const paid = p.status === 'paid';
    const detail = paid ? `${tidy(p.pays)} DOGE is on its way; it confirms in the next Dogecoin block, usually within a minute.`
      : final ? 'The bridge pays it within seconds.' : 'Waiting for it to be final on DogecoinVM, about two seconds.';
    const c = card(`Withdrawing ${w.amount} DOGE to Dogecoin`,
      steps([['Final on DogecoinVM', final], ['Paid on Dogecoin', paid], ['In a Dogecoin block', false]]), detail);
    cards.push(paid ? dogeWaiting(c) : c);
  }

  for (const o of outgoing()) {
    if (o.network !== 'doge' || o.kind === 'withdraw') continue;
    if (o.kind === 'move' && depositTxids.has(o.txid)) continue; // the deposit card covers it
    const change = BigInt(o.change);
    const back = change > 0n ? ` ${chain.formatDoge(change)} DOGE change comes back when it confirms.` : '';
    const title = o.kind === 'move' ? `Moving ${chain.formatDoge(BigInt(o.amount))} DOGE to DogecoinVM`
      : `Sending ${chain.formatDoge(BigInt(o.amount))} DOGE on Dogecoin`;
    cards.push(dogeWaiting(card(title, steps([['Sent', true], ['In a Dogecoin block', false]]),
      `Waiting for a Dogecoin block, usually within a minute.${back}`)));
  }

  // Dogecoin blocks come at random. When one is slow, say so rather than
  // leave a countdown that looks stuck.
  const quiet = dogeBlockTime ? Math.floor((Date.now() / 1000 - dogeBlockTime) / 60) : 0;
  if (quiet >= 4) {
    for (const c of cards.filter((c) => c.dataset.dogecoin)) {
      const p = document.createElement('p');
      p.className = 'inflight-quiet';
      p.textContent = `Dogecoin hasn't found a block for ${quiet} minutes. Blocks average a minute but come at random; this continues when the next one arrives.`;
      c.append(p);
    }
  }
  $('inflight-list').replaceChildren(...cards);
  $('inflight').hidden = cards.length === 0;
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
    showFailure($('send-result'), err);
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
    const { txid, unknown } = await pay(script, amount, undefined, undefined, 'doge', { deposit: expected });
    if (unknown) {
      showResult($('move-result'), unknownOutcome, false, txid, 'doge');
    } else {
      const n = confirmationsFor(info, amount);
      showResult($('move-result'),
        `Sent to your deposit address. It's credited on DogecoinVM after ${n} Dogecoin confirmation${n === 1 ? '' : 's'}, about ${n === 1 ? 'a minute' : `${n} minutes`}. Transaction:`, true, txid, 'doge');
    }
    $('move-amount').value = '';
    setTimeout(refreshDeposits, 3000);
  } catch (err) {
    showFailure($('move-result'), err);
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
  if (!key) return;
  const gen = generation;
  try {
    const deposits = await api(`/api/deposits/${myAddress()}`);
    if (gen !== generation) return;
    lastDeposits = deposits;
    renderInflight();
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
    }, 'vm', { reserve: info.reserveAddress, withdrawTo: chain.encodeAddress(to, info.dogecoinVersions) });
    if (unknown) showResult($('withdraw-result'), unknownOutcome, false, txid);
    else showResult($('withdraw-result'), 'Withdrawal sent. The bridge pays out once it is in a block. Transaction:', true, txid);
    $('withdraw-form').reset();
  } catch (err) {
    // The bridge refused it, so it will never be paid; forget it.
    if (err.rejected && pendingTxid) saveWithdrawals(savedWithdrawals().filter((w) => w.txid !== pendingTxid));
    showFailure($('withdraw-result'), err);
  } finally {
    button.disabled = false;
    renderWithdrawals();
    refreshWithdrawalStatus();
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
  const stream = listenForBlocks();
  // A fallback for when the event stream is down; while it's up, blocks
  // drive the refreshes and this only keeps the status (a pause, sync
  // progress) current.
  setInterval(() => {
    refreshStatus();
    if (stream.readyState === EventSource.OPEN) return;
    refreshWallet();
    refreshDogeWallet();
    refreshDeposits();
    refreshWithdrawalStatus();
    if (!$('panel-withdraw').hidden) renderWithdrawals();
  }, 15000);
}

// soon runs fn once for any number of calls within 100 ms, so a burst of
// events makes one set of requests.
const scheduled = new Map();
function soon(fn) {
  if (scheduled.has(fn)) return;
  scheduled.set(fn, setTimeout(() => { scheduled.delete(fn); fn(); }, 100));
}

// listenForBlocks refreshes the wallet the moment either chain has a new
// block. On DogecoinVM a block is final once accepted, so a payment shows
// as soon as it is final. The browser reconnects by itself if it drops.
function listenForBlocks() {
  const stream = new EventSource('/api/events');
  stream.addEventListener('block', (e) => {
    let chain;
    try { ({ chain } = JSON.parse(e.data)); } catch { return; }
    soon(refreshStatus);
    if (chain === 'dogecoinvm') {
      soon(refreshWallet); // payments, and deposits being credited
      soon(refreshDeposits);
    } else if (chain === 'dogecoin') {
      soon(refreshDogeWallet);
      soon(refreshDeposits); // confirmations counting up
    }
    soon(refreshWithdrawalStatus); // final, paid, then in a Dogecoin block
    if (!$('panel-withdraw').hidden) soon(renderWithdrawals);
  });
  return stream;
}

// Transaction, block and address pages used to open on this page; they're
// on the explorer now.
if (/^#\/(tx|block|address)\//.test(location.hash)) location.replace(`/explorer${location.hash}`);
else start();
