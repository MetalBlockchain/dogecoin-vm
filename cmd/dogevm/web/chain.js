// Keys, addresses and transactions for DogecoinVM and Dogecoin, in the
// browser. Transactions are legacy (pre-SegWit) format, which both chains use.
import * as secp from './vendor/noble-secp256k1-2.3.0/index.js';
import { sha256 } from './vendor/noble-hashes-1.8.0/sha2.js';
import { ripemd160 } from './vendor/noble-hashes-1.8.0/legacy.js';

export const KOINU = 100_000_000n;

// Dogecoin's recommended 0.01 DOGE/kB wallet fee, and soft dust limit.
const FEE_PER_BYTE = 1000n;
const SOFT_DUST = KOINU / 100n;

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

// parseKey accepts a WIF (any network) or 64 hex characters.
export function parseKey(s) {
  s = s.trim();
  if (/^[0-9a-f]{64}$/i.test(s)) return unhex(s);
  const { payload } = checkDecode(s);
  if (payload.length === 33 && payload[32] === 1) return payload.slice(0, 32);
  if (payload.length === 32) return payload;
  throw new Error('not a private key');
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

function opReturn(data) {
  return { value: 0n, script: concat(Uint8Array.of(0x6a), pushData(data)) };
}

// buildPayment spends key's P2PKH outputs (from the API's utxo list) to pay
// amount to script, with an optional OP_RETURN, returning the signed hex and
// fee.
export async function buildPayment({ key, utxos, script, amount, data }) {
  const from = keyDestination(key);
  const fromScript = pkScript(from);
  const spendable = utxos
    .filter((u) => u.confirmations > 0 && u.script === hex(fromScript))
    .map((u) => ({ ...u, value: BigInt(u.value) }))
    .sort((a, b) => (b.value > a.value ? 1 : -1));

  const outputs = [{ value: amount, script }];
  if (data) outputs.push(opReturn(data));

  let inputs = [];
  let total = 0n;
  let fee = 0n;
  for (const u of spendable) {
    inputs.push(u);
    total += u.value;
    const size = 10 + 149 * inputs.length + 34 * (outputs.length + 1) + (data ? data.length + 3 : 0);
    fee = BigInt(size) * FEE_PER_BYTE;
    if (total >= amount + fee) break;
  }
  if (total < amount + fee) {
    throw new Error(`not enough confirmed DOGE: have ${formatDoge(total)}, need ${formatDoge(amount + fee)} including the fee`);
  }
  const change = total - amount - fee;
  if (change >= SOFT_DUST) outputs.push({ value: change, script: fromScript });
  else fee += change;

  const tx = {
    version: 1,
    inputs: inputs.map((u) => ({ txid: u.txid, vout: u.vout, script: new Uint8Array() })),
    outputs,
  };
  const pub = secp.getPublicKey(key, true);
  for (let i = 0; i < tx.inputs.length; i++) {
    const sig = await secp.signAsync(sighash(tx, i, fromScript), key, { lowS: true });
    tx.inputs[i].script = concat(pushData(concat(derSignature(sig), Uint8Array.of(1))), pushData(pub));
  }
  return { hex: hex(serialize(tx)), fee };
}

// pegOutData is the DVMO tag naming the Dogecoin address to pay.
export function pegOutData(dogeDest) {
  return concat(new TextEncoder().encode('DVMO'), Uint8Array.of(dogeDest.kind), dogeDest.hash);
}
