// Keys, addresses and transactions for DogecoinVM and Dogecoin, in the
// browser. Transactions are legacy (pre-SegWit) format, which both chains use.
import * as secp from './vendor/noble-secp256k1-2.3.0/index.js';
import { sha256 } from './vendor/noble-hashes-1.8.0/sha2.js';
import { ripemd160 } from './vendor/noble-hashes-1.8.0/legacy.js';

export const KOINU = 100_000_000n;

// Dogecoin's recommended 0.01 DOGE/kB wallet fee, its soft dust limit (each
// output below it costs that much again in fee) and hard dust limit (outputs
// below it are not relayed).
const FEE_PER_BYTE = 1000n;
// DogecoinVM's relay minimum, 0.001 DOGE/kB, a tenth of that. Blocks there
// have room to spare, so the minimum always makes the next block; on
// Dogecoin, which can be busy, wallets pay the recommended rate.
export const VM_FEE_PER_BYTE = 100n;
const SOFT_DUST = KOINU / 100n;
const HARD_DUST = SOFT_DUST / 10n;

// No payment this page builds should cost more than this in fees; a larger
// figure means something is wrong, so refuse to sign.
const MAX_FEE = 5n * KOINU;

const concat = (...parts) => {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let i = 0;
  for (const p of parts) { out.set(p, i); i += p.length; }
  return out;
};
export const hex = (bytes) => Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
export const unhex = (s) => {
  if (!/^([0-9a-f]{2})*$/i.test(s)) throw new Error('not hex');
  return Uint8Array.from(s.match(/../g) || [], (b) => parseInt(b, 16));
};
const dsha = (b) => sha256(sha256(b));
const hash160 = (b) => ripemd160(sha256(b));

// --- base58check ---------------------------------------------------------

const B58 = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

function b58encode(bytes) {
  let n = BigInt('0x' + (hex(bytes) || '0'));
  let out = '';
  while (n > 0n) { out = B58[Number(n % 58n)] + out; n /= 58n; }
  for (const b of bytes) { if (b !== 0) break; out = '1' + out; }
  return out;
}

function b58decode(s) {
  let n = 0n;
  for (const c of s) {
    const v = B58.indexOf(c);
    if (v < 0) throw new Error(`invalid character "${c}"`);
    n = n * 58n + BigInt(v);
  }
  let h = n.toString(16);
  if (h.length % 2) h = '0' + h;
  const body = n === 0n ? new Uint8Array() : unhex(h);
  const zeros = s.match(/^1*/)[0].length;
  return concat(new Uint8Array(zeros), body);
}

function checkEncode(version, payload) {
  const data = concat(Uint8Array.of(version), payload);
  return b58encode(concat(data, dsha(data).slice(0, 4)));
}

function checkDecode(s) {
  const raw = b58decode(s.trim());
  if (raw.length < 5) throw new Error('too short');
  const data = raw.slice(0, -4);
  const sum = dsha(data).slice(0, 4);
  if (hex(sum) !== hex(raw.slice(-4))) throw new Error('checksum mismatch: check for a typo');
  return { version: data[0], payload: data.slice(1) };
}

// --- addresses and scripts ----------------------------------------------

// decodeAddress returns {kind: 0 (P2PKH) | 1 (P2SH), hash} for an address on
// the network with the given version bytes.
export function decodeAddress(address, versions) {
  const { version, payload } = checkDecode(address);
  if (payload.length !== 20) throw new Error('not a pay-to-hash address');
  if (version === versions.p2pkh) return { kind: 0, hash: payload };
  if (version === versions.p2sh) return { kind: 1, hash: payload };
  throw new Error('address is for a different network');
}

export const encodeAddress = (dest, versions) =>
  checkEncode(dest.kind === 1 ? versions.p2sh : versions.p2pkh, dest.hash);

export function pkScript(dest) {
  return dest.kind === 1
    ? concat(Uint8Array.of(0xa9, 0x14), dest.hash, Uint8Array.of(0x87))
    : concat(Uint8Array.of(0x76, 0xa9, 0x14), dest.hash, Uint8Array.of(0x88, 0xac));
}

function pushData(data) {
  if (data.length < 0x4c) return concat(Uint8Array.of(data.length), data);
  if (data.length <= 0xff) return concat(Uint8Array.of(0x4c, data.length), data);
  throw new Error('push too large');
}

// depositRedeemScript mirrors cmd/dogevm: <kind || hash160> OP_DROP, then
// the m-of-n multisig of the peg signers.
export function depositRedeemScript(dest, signers) {
  const keys = signers.publicKeys.map(unhex);
  const multisig = concat(
    Uint8Array.of(0x50 + signers.required),
    ...keys.map(pushData),
    Uint8Array.of(0x50 + keys.length, 0xae),
  );
  return concat(Uint8Array.of(0x15, dest.kind), dest.hash, Uint8Array.of(0x75), multisig);
}

// depositAddress computes dest's personal Dogecoin deposit address from the
// signers' public keys, so the page can check what the server says.
export const depositAddress = (dest, signers, dogeVersions) =>
  encodeAddress({ kind: 1, hash: hash160(depositRedeemScript(dest, signers)) }, dogeVersions);

// --- keys ----------------------------------------------------------------

export function newPrivateKey() {
  return secp.utils.randomPrivateKey();
}

// parseKey accepts a compressed-key WIF (any network) or 64 hex characters.
// Addresses here are for compressed public keys, so an uncompressed WIF
// would open a different, empty wallet; it is refused.
export function parseKey(s) {
  s = s.trim();
  let key;
  if (/^[0-9a-f]{64}$/i.test(s)) {
    key = unhex(s);
  } else {
    const { payload } = checkDecode(s);
    if (payload.length === 32) throw new Error('this is an uncompressed-key WIF; export a compressed one');
    if (payload.length !== 33 || payload[32] !== 1) throw new Error('not a private key');
    key = payload.slice(0, 32);
  }
  if (!secp.utils.isValidPrivateKey(key)) throw new Error('not a valid private key');
  return key;
}

export const wif = (key, versions) => checkEncode(versions.wif, concat(key, Uint8Array.of(1)));

export function keyDestination(key) {
  return { kind: 0, hash: hash160(secp.getPublicKey(key, true)) };
}

// --- amounts ---------------------------------------------------------------

export function parseDoge(s) {
  const m = /^(\d{1,11})(?:\.(\d{1,8}))?$/.exec(s.trim());
  if (!m) throw new Error('enter an amount like 12.5');
  return BigInt(m[1]) * KOINU + BigInt((m[2] || '').padEnd(8, '0'));
}

export function formatDoge(koinu) {
  koinu = BigInt(koinu);
  const whole = koinu / KOINU;
  const frac = (koinu % KOINU).toString().padStart(8, '0').replace(/0+$/, '');
  return whole.toLocaleString('en-US') + (frac ? '.' + frac : '');
}

// --- transactions --------------------------------------------------------

const u32 = (n) => { const b = new Uint8Array(4); new DataView(b.buffer).setUint32(0, n, true); return b; };
const u64 = (n) => { const b = new Uint8Array(8); new DataView(b.buffer).setBigUint64(0, BigInt(n), true); return b; };
function varint(n) {
  if (n < 0xfd) return Uint8Array.of(n);
  if (n <= 0xffff) return Uint8Array.of(0xfd, n & 0xff, n >> 8);
  return concat(Uint8Array.of(0xfe), u32(n));
}

function serialize(tx) {
  return concat(
    u32(tx.version),
    varint(tx.inputs.length),
    ...tx.inputs.flatMap((i) => [
      unhex(i.txid).reverse(), u32(i.vout), varint(i.script.length), i.script, u32(0xffffffff),
    ]),
    varint(tx.outputs.length),
    ...tx.outputs.flatMap((o) => [u64(o.value), varint(o.script.length), o.script]),
    u32(0),
  );
}

// Legacy SIGHASH_ALL: the input being signed carries the script it spends.
function sighash(tx, index, prevScript) {
  const copy = {
    ...tx,
    inputs: tx.inputs.map((i, n) => ({ ...i, script: n === index ? prevScript : new Uint8Array() })),
  };
  return dsha(concat(serialize(copy), u32(1)));
}

// derSignature encodes a (low-S) signature in the strict DER form consensus
// requires.
function derSignature(sig) {
  const int = (n) => {
    let b = unhex(n.toString(16).padStart(64, '0'));
    let i = 0;
    while (i < b.length - 1 && b[i] === 0 && b[i + 1] < 0x80) i++;
    b = b.slice(i);
    if (b[0] & 0x80) b = concat(Uint8Array.of(0), b);
    return concat(Uint8Array.of(0x02, b.length), b);
  };
  const body = concat(int(sig.r), int(sig.s));
  return concat(Uint8Array.of(0x30, body.length), body);
}

// txid is a transaction's id: its double SHA-256, byte-reversed.
export const txid = (raw) => hex(dsha(raw).reverse());

// parseOutputs reads the outputs of a legacy-format transaction.
function parseOutputs(raw) {
  let i = 4;
  const view = new DataView(raw.buffer, raw.byteOffset, raw.byteLength);
  const need = (n) => { if (i + n > raw.length) throw new Error('truncated transaction'); };
  const readVarint = () => {
    need(1);
    const b = raw[i++];
    if (b < 0xfd) return b;
    if (b === 0xfd) { need(2); const v = view.getUint16(i, true); i += 2; return v; }
    if (b === 0xfe) { need(4); const v = view.getUint32(i, true); i += 4; return v; }
    throw new Error('transaction too large');
  };
  const inputs = readVarint();
  for (let n = 0; n < inputs; n++) {
    need(36); i += 36;
    const len = readVarint();
    need(len + 4); i += len + 4;
  }
  const count = readVarint();
  const outputs = [];
  for (let n = 0; n < count; n++) {
    need(8);
    const value = view.getBigUint64(i, true); i += 8;
    const len = readVarint();
    need(len);
    outputs.push({ value, script: raw.slice(i, i + len) });
    i += len;
  }
  return outputs;
}

function opReturn(data) {
  return { value: 0n, script: concat(Uint8Array.of(0x6a), pushData(data)) };
}

// verifiedInput checks a UTXO the server listed against the transaction that
// created it, fetched with getRawTx and matched to its id, and returns the
// value and script from those bytes. Legacy signatures do not commit to the
// amounts they spend, so a server lying about a value could otherwise turn
// the difference into fee.
async function verifiedInput(u, getRawTx, fromScript) {
  const raw = unhex(await getRawTx(u.txid));
  if (txid(raw) !== u.txid) throw new Error(`the server sent the wrong transaction for ${u.txid}`);
  const out = parseOutputs(raw)[u.vout];
  if (!out) throw new Error(`transaction ${u.txid} has no output ${u.vout}`);
  if (hex(out.script) !== hex(fromScript)) throw new Error(`output ${u.txid}:${u.vout} is not yours`);
  return { txid: u.txid, vout: u.vout, value: out.value };
}

// planPayment chooses which of key's P2PKH outputs (from the API's utxo
// list, each checked with getRawTx) pay amount to script, with an optional
// OP_RETURN, at feePerByte (koinu), and returns the unsigned transaction: its inputs, outputs, the
// total of the inputs, the fee, and the unsigned bytes to review.
export async function planPayment({ key, utxos, getRawTx, script, amount, data, feePerByte = FEE_PER_BYTE }) {
  if (amount < HARD_DUST) throw new Error(`the smallest payment is ${formatDoge(HARD_DUST)} DOGE`);
  const from = keyDestination(key);
  const fromScript = pkScript(from);
  const spendable = utxos
    .filter((u) => u.confirmations > 0 && u.script === hex(fromScript))
    .map((u) => ({ ...u, value: BigInt(u.value) }))
    .sort((a, b) => (b.value > a.value ? 1 : -1));

  const outputs = [{ value: amount, script }];
  if (data) outputs.push(opReturn(data));
  const dustFee = amount < SOFT_DUST ? SOFT_DUST : 0n;

  let inputs = [];
  let total = 0n;
  let fee = 0n;
  for (const u of spendable) {
    const input = await verifiedInput(u, getRawTx, fromScript);
    inputs.push(input);
    total += input.value;
    const size = 10 + 149 * inputs.length + 34 * (outputs.length + 1) + (data ? data.length + 3 : 0);
    fee = BigInt(size) * feePerByte + dustFee;
    if (total >= amount + fee) break;
  }
  if (total < amount + fee) {
    throw new Error(`not enough confirmed DOGE: have ${formatDoge(total)}, need ${formatDoge(amount + fee)} including the fee`);
  }
  const change = total - amount - fee;
  if (change >= SOFT_DUST) outputs.push({ value: change, script: fromScript });
  else fee += change;
  if (fee > MAX_FEE) throw new Error(`the fee would be ${formatDoge(fee)} DOGE; refusing to sign`);

  const tx = {
    version: 1,
    inputs: inputs.map((u) => ({ txid: u.txid, vout: u.vout, script: new Uint8Array() })),
    outputs,
  };
  return { tx, inputTotal: total, fee, unsignedHex: hex(serialize(tx)) };
}

// signPlan signs a planned payment, and checks the signed transaction pays
// exactly the outputs that were reviewed. It returns the signed hex, its
// txid and the fee.
export async function signPlan(plan, key) {
  const fromScript = pkScript(keyDestination(key));
  const tx = { ...plan.tx, inputs: plan.tx.inputs.map((i) => ({ ...i })) };
  const pub = secp.getPublicKey(key, true);
  for (let i = 0; i < tx.inputs.length; i++) {
    const sig = await secp.signAsync(sighash(tx, i, fromScript), key, { lowS: true });
    tx.inputs[i].script = concat(pushData(concat(derSignature(sig), Uint8Array.of(1))), pushData(pub));
  }
  const raw = serialize(tx);
  const outs = (bytes) => JSON.stringify(parseOutputs(bytes).map((o) => [String(o.value), hex(o.script)]));
  if (outs(raw) !== outs(unhex(plan.unsignedHex))) throw new Error('the signed transaction differs from the one reviewed; nothing was sent');
  return { hex: hex(raw), txid: txid(raw), fee: plan.fee };
}

// buildPayment plans and signs a payment in one step.
export async function buildPayment(args) {
  return signPlan(await planPayment(args), args.key);
}

// reviewOutputs decodes a transaction's outputs from its bytes, for showing
// before it is signed: each is {value, script, address} for a pay-to-hash
// output, {value, script, data} for OP_RETURN, or {value, script} otherwise.
export function reviewOutputs(txHex, versions) {
  return parseOutputs(unhex(txHex)).map((o) => {
    const s = o.script;
    if (s.length === 25 && s[0] === 0x76 && s[1] === 0xa9 && s[2] === 0x14 && s[23] === 0x88 && s[24] === 0xac) {
      return { ...o, address: encodeAddress({ kind: 0, hash: s.slice(3, 23) }, versions) };
    }
    if (s.length === 23 && s[0] === 0xa9 && s[1] === 0x14 && s[22] === 0x87) {
      return { ...o, address: encodeAddress({ kind: 1, hash: s.slice(2, 22) }, versions) };
    }
    if (s[0] === 0x6a && s.length >= 2 && s[1] < 0x4c && s.length === 2 + s[1]) return { ...o, data: s.slice(2) };
    return o;
  });
}

// pegOutDestination reads the Dogecoin address from a DVMO tag, or returns
// null if data isn't one.
export function pegOutDestination(data, dogeVersions) {
  const tag = new TextEncoder().encode('DVMO');
  if (data.length !== 25 || tag.some((b, i) => data[i] !== b) || data[4] > 1) return null;
  return encodeAddress({ kind: data[4], hash: data.slice(5) }, dogeVersions);
}

// pegOutData is the DVMO tag naming the Dogecoin address to pay.
export function pegOutData(dogeDest) {
  return concat(new TextEncoder().encode('DVMO'), Uint8Array.of(dogeDest.kind), dogeDest.hash);
}
