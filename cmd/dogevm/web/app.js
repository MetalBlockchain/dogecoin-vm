import * as chain from './chain.js';

const $ = (id) => document.getElementById(id);
const KEY_STORE = 'dogevm.key';
const WITHDRAW_STORE = 'dogevm.withdrawals';

let info = null;
let key = null; // Uint8Array, or null
let depositShownFor = null;

// Stored values survive reloads; storage can be unavailable (private mode).
const store = {
  get(name) { try { return localStorage.getItem(name); } catch { return null; } },
  set(name, value) { try { localStorage.setItem(name, value); } catch { /* ignore */ } },
  remove(name) { try { localStorage.removeItem(name); } catch { /* ignore */ } },
};

async function api(path, body) {
  const res = await fetch(path, body === undefined ? {} : {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || `request failed (${res.status})`);
  return data;
}

// API amounts are DOGE decimal strings with 8 places; show them tidily.
const tidy = (s) => chain.formatDoge(chain.parseDoge(String(s).replace('-', '')));
const short = (txid) => `${txid.slice(0, 10)}…${txid.slice(-6)}`;

function showResult(el, message, ok) {
  el.textContent = message;
  el.className = 'result ' + (ok ? 'ok' : 'error');
}

// --- tabs ------------------------------------------------------------------

function selectTab(name) {
  for (const tab of document.querySelectorAll('[role=tab]')) {
    const selected = tab.id === `tab-${name}`;
    tab.setAttribute('aria-selected', String(selected));
    $(tab.getAttribute('aria-controls')).hidden = !selected;
  }
  if (name === 'deposit') showDeposit();
  if (name === 'withdraw') renderWithdrawals();
}

for (const tab of document.querySelectorAll('[role=tab]')) {
  tab.addEventListener('click', () => selectTab(tab.id.replace('tab-', '')));
}

document.addEventListener('click', async (e) => {
  const target = e.target.closest('button.copy');
  if (!target) return;
  await navigator.clipboard.writeText($(target.dataset.copy).textContent);
  target.textContent = 'Copied';
  setTimeout(() => { target.textContent = 'Copy'; }, 1500);
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
  if (info.faucet.enabled) {
    $('faucet-text').textContent =
      `The faucet sends ${tidy(info.faucet.amount)} testnet DOGE straight to your DogecoinVM address, once a day.`;
  } else {
    $('faucet-text').textContent = 'This network has no faucet. Deposit Dogecoin testnet DOGE on the Deposit tab instead.';
    $('faucet-claim').hidden = true;
  }
}

async function refreshStatus() {
  try {
    const s = await api('/api/status');
    $('doge-height').textContent = s.dogecoinHeight.toLocaleString('en-US');
    $('vm-height').textContent = s.dogecoinvmHeight.toLocaleString('en-US');
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
    $('verdict').textContent = a.solvent
      ? 'Fully backed.'
      : 'Not fully backed: the bridge has stopped moving DOGE.';
    $('verdict').className = 'peg-verdict' + (a.solvent ? '' : ' bad');
  } catch (err) {
    $('verdict').textContent = `Can't reach the bridge: ${err.message}`;
    $('verdict').className = 'peg-verdict bad';
  }
}

// --- key and wallet ------------------------------------------------------------

const myDest = () => chain.keyDestination(key);
const myAddress = () => chain.encodeAddress(myDest(), info.dogecoinvmVersions);

function setKey(newKey) {
  key = newKey;
  if (key) store.set(KEY_STORE, chain.hex(key));
  else store.remove(KEY_STORE);
  depositShownFor = null;
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
  $('wif-vm').textContent = chain.wif(key, info.dogecoinvmVersions);
  $('wif-doge').textContent = chain.wif(key, info.dogecoinVersions);
  refreshWallet();
}

let utxos = [];

async function refreshWallet() {
  if (!key) return;
  try {
    const a = await api(`/api/address/${myAddress()}`);
    utxos = a.utxos;
    $('balance').textContent = tidy(a.confirmed);
    const pending = chain.parseDoge(a.pending);
    $('balance-pending').textContent = pending > 0n ? `${tidy(a.pending)} DOGE arriving in the next block` : '';
    const list = $('history');
    list.replaceChildren();
    if (a.history.length === 0) {
      const li = document.createElement('li');
      li.className = 'empty';
      li.textContent = 'Nothing yet. Get DOGE from the faucet or deposit it from Dogecoin.';
      list.append(li);
    }
    for (const h of a.history) {
      const li = document.createElement('li');
      const received = chain.parseDoge(h.received);
      li.innerHTML = `<code></code><span></span>`;
      li.firstChild.textContent = short(h.txid);
      li.lastChild.textContent = (received > 0n ? `+${tidy(h.received)} DOGE` : 'Sent')
        + (h.confirmations > 0 ? '' : ' (pending)');
      list.append(li);
    }
  } catch (err) {
    $('balance').textContent = '…';
    $('balance-pending').textContent = `Can't load your balance: ${err.message}`;
  }
}

$('create-key').addEventListener('click', () => setKey(chain.newPrivateKey()));

$('import-form').addEventListener('submit', (e) => {
  e.preventDefault();
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

// pay signs and broadcasts a payment, then refreshes the wallet.
async function pay(script, amount, data) {
  const { hex } = await chain.buildPayment({ key, utxos, script, amount, data });
  const { txid } = await api('/api/tx', { hex });
  setTimeout(refreshWallet, 1500);
  return txid;
}

$('send-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const button = e.submitter;
  button.disabled = true;
  try {
    const to = chain.decodeAddress($('send-to').value, info.dogecoinvmVersions);
    const amount = chain.parseDoge($('send-amount').value);
    const txid = await pay(chain.pkScript(to), amount);
    showResult($('send-result'), `Sent. Transaction ${short(txid)}.`, true);
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
  const addr = myAddress();
  if (depositShownFor !== addr) {
    try {
      const d = await api('/api/deposit-address', { address: addr });
      const expected = chain.depositAddress(myDest(), info.signers, info.dogecoinVersions);
      $('deposit-address').textContent = d.depositAddress;
      $('deposit-verified').textContent = d.depositAddress === expected
        ? `Checked in your browser: only the ${info.signers.required}-of-${info.signers.publicKeys.length} peg signers can spend it, and deposits credit your address.`
        : 'Warning: this address does not match the peg signers. Do not send to it.';
      const { default: qrcode } = await import('./vendor/qrcode-generator-2.0.4/qrcode.mjs');
      const qr = qrcode(0, 'M');
      qr.addData(d.depositAddress);
      qr.make();
      $('deposit-qr').innerHTML = qr.createSvgTag({ cellSize: 4, margin: 0, scalable: true });
      depositShownFor = addr;
    } catch (err) {
      $('deposit-address').textContent = `Can't get a deposit address: ${err.message}`;
      return;
    }
  }
  refreshDeposits();
}

async function refreshDeposits() {
  if (!key || depositShownFor === null) return;
  try {
    const deposits = await api(`/api/deposits/${myAddress()}`);
    const list = $('deposits');
    list.replaceChildren();
    if (deposits.length === 0) {
      const li = document.createElement('li');
      li.className = 'empty';
      li.textContent = 'No deposits yet. They show up here once Dogecoin sees them.';
      list.append(li);
    }
    for (const d of deposits) {
      const li = document.createElement('li');
      li.innerHTML = '<span></span><span></span>';
      li.firstChild.textContent = `${tidy(d.amount)} DOGE`;
      if (d.creditTxid) {
        li.lastChild.textContent = `Credited ${tidy(d.credited)} DOGE`;
        li.lastChild.className = 'status-done';
      } else {
        li.lastChild.textContent = `${Math.min(d.confirmations, d.required)} of ${d.required} confirmations`;
        li.lastChild.className = 'status-waiting';
      }
      list.append(li);
    }
  } catch { /* try again on the next poll */ }
}

// --- withdraw ------------------------------------------------------------------

const savedWithdrawals = () => JSON.parse(store.get(WITHDRAW_STORE) || '[]');

$('withdraw-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const button = e.submitter;
  button.disabled = true;
  try {
    const to = chain.decodeAddress($('withdraw-to').value, info.dogecoinVersions);
    const amount = chain.parseDoge($('withdraw-amount').value);
    if (amount < chain.parseDoge(info.minPegOut)) throw new Error(`The minimum withdrawal is ${tidy(info.minPegOut)} DOGE.`);
    const reserve = chain.decodeAddress(info.reserveAddress, info.dogecoinvmVersions);
    const txid = await pay(chain.pkScript(reserve), amount, chain.pegOutData(to));
    const saved = savedWithdrawals();
    saved.unshift({ txid, to: $('withdraw-to').value.trim(), amount: chain.formatDoge(amount) });
    store.set(WITHDRAW_STORE, JSON.stringify(saved.slice(0, 20)));
    showResult($('withdraw-result'), `Withdrawal sent. The bridge pays out once it is in a block.`, true);
    $('withdraw-form').reset();
    renderWithdrawals();
  } catch (err) {
    showResult($('withdraw-result'), err.message, false);
  } finally {
    button.disabled = false;
  }
});

async function renderWithdrawals() {
  const list = $('withdrawals');
  const saved = savedWithdrawals();
  list.replaceChildren();
  for (const w of saved) {
    const li = document.createElement('li');
    li.innerHTML = '<span></span><span></span>';
    li.firstChild.textContent = `${w.amount} DOGE to ${w.to.slice(0, 8)}…`;
    li.lastChild.textContent = 'Checking…';
    li.lastChild.className = 'status-waiting';
    list.append(li);
    api(`/api/pegout/${w.txid}`).then((p) => {
      if (p.status === 'paid') {
        li.lastChild.textContent = `Paid ${tidy(p.pays)} DOGE on Dogecoin`;
        li.lastChild.className = 'status-done';
      } else {
        li.lastChild.textContent = 'Waiting for the bridge';
      }
    }).catch(() => { li.lastChild.textContent = 'Status unavailable'; });
  }
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

async function start() {
  try {
    await loadInfo();
  } catch (err) {
    $('verdict').textContent = `Can't reach the bridge: ${err.message}`;
    $('verdict').className = 'peg-verdict bad';
    return;
  }
  const saved = store.get(KEY_STORE);
  if (saved) {
    try { key = chain.unhex(saved); } catch { store.remove(KEY_STORE); }
  }
  renderKey();
  refreshStatus();
  setInterval(() => {
    refreshStatus();
    refreshWallet();
    refreshDeposits();
    if (!$('panel-withdraw').hidden) renderWithdrawals();
  }, 15000);
}

start();
