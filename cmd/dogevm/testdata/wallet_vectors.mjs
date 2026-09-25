// Generates wallet test vectors from the web wallet's chain.js, the reference
// implementation. TestWalletVectors checks every signed transaction against
// btcd's script engine and every address against the Go code, and writes
// testdata/wallet-vectors.json. Other wallets (the macOS app's Rust core)
// must reproduce these byte for byte.
//
// Test keys are sha256 of public labels: this file holds no secret.
//
//   node testdata/wallet_vectors.mjs > testdata/wallet-vectors.json
import * as chain from '../web/chain.js';
import { sha256 } from '../web/vendor/noble-hashes-1.8.0/sha2.js';

const enc = new TextEncoder();
const keyFor = (label) => sha256(enc.encode(label));

// Mainnet address versions, the same on Dogecoin and DogecoinVM.
const versions = { p2pkh: 30, p2sh: 22, wif: 158 };

// A minimal legacy transaction paying outputs, for inputs to spend.
function u32(n) { const b = new Uint8Array(4); new DataView(b.buffer).setUint32(0, n, true); return b; }
function u64(n) { const b = new Uint8Array(8); new DataView(b.buffer).setBigUint64(0, BigInt(n), true); return b; }
function cat(...parts) { const o = new Uint8Array(parts.reduce((n, p) => n + p.length, 0)); let i = 0; for (const p of parts) { o.set(p, i); i += p.length; } return o; }
function prevTx(tag, outputs) {
  const input = cat(new Uint8Array(32).fill(tag), u32(0), Uint8Array.of(1), Uint8Array.of(0x51), u32(0xffffffff));
  const outs = outputs.map((o) => cat(u64(o.value), Uint8Array.of(o.script.length), o.script));
  return cat(u32(1), Uint8Array.of(1), input, Uint8Array.of(outputs.length), ...outs, u32(0));
}

const keys = ['dogevm vector key 1', 'dogevm vector key 2', 'dogevm vector key 3'].map((label) => {
  const key = keyFor(label);
  const dest = chain.keyDestination(key);
  return {
    label, key, dest,
    vector: {
      label,
      hash160: chain.hex(dest.hash),
      address: chain.encodeAddress(dest, versions),
      wif: chain.wif(key, versions),
    },
  };
});

// The signer set: the three keys' compressed public keys, 2 of 3.
const { getPublicKey } = await import('../web/vendor/noble-secp256k1-2.3.0/index.js');
const signers = { required: 2, publicKeys: keys.map((k) => chain.hex(getPublicKey(k.key, true))) };
const deposit = {
  signers,
  dest: { kind: 0, hash160: keys[0].vector.hash160 },
  redeemScript: chain.hex(chain.depositRedeemScript(keys[0].dest, signers)),
  address: chain.depositAddress(keys[0].dest, signers, versions),
};
const reserve = chain.decodeAddress(chain.encodeAddress({ kind: 1, hash: (await import('../web/vendor/noble-hashes-1.8.0/legacy.js')).ripemd160(sha256(
  // The peg multisig: OP_2 <3 keys> OP_3 OP_CHECKMULTISIG.
  cat(Uint8Array.of(0x52), ...signers.publicKeys.map((h) => cat(Uint8Array.of(33), chain.unhex(h))), Uint8Array.of(0x53, 0xae)),
)) }, versions), versions);

// Payments from key 1.
const from = keys[0];
const fromScript = chain.pkScript(from.dest);
const D = 100_000_000n;
const cases = [
  { name: 'one input with change', coins: [500n * D], to: chain.pkScript(keys[1].dest), amount: 120n * D },
  { name: 'several inputs, largest first', coins: [3n * D, 5n * D, 7n * D], to: chain.pkScript(keys[1].dest), amount: 11n * D },
  { name: 'withdrawal with a DVMO tag', coins: [50n * D], to: chain.pkScript(reserve), amount: 20n * D, data: chain.pegOutData(keys[2].dest) },
  { name: 'deposit to a personal deposit address', coins: [30n * D], to: chain.pkScript(chain.decodeAddress(deposit.address, versions)), amount: 10n * D },
  { name: 'below the soft dust limit pays the surcharge', coins: [2n * D], to: chain.pkScript(keys[1].dest), amount: D / 200n },
  { name: 'change below the soft dust limit goes to the fee', coins: [D + D / 100n], to: chain.pkScript(keys[1].dest), amount: D },
  // DogecoinVM payments pay the relay minimum rather than Dogecoin's
  // recommended rate.
  { name: 'DogecoinVM payment at the relay minimum', coins: [3n * D, 5n * D], to: chain.pkScript(keys[1].dest), amount: 6n * D, feePerByte: chain.VM_FEE_PER_BYTE },
  { name: 'DogecoinVM withdrawal at the relay minimum', coins: [50n * D], to: chain.pkScript(reserve), amount: 20n * D, data: chain.pegOutData(keys[2].dest), feePerByte: chain.VM_FEE_PER_BYTE },
];

const payments = [];
for (const [n, c] of cases.entries()) {
  const raw = prevTx(n + 1, c.coins.map((value) => ({ value, script: fromScript })));
  const prevTxid = chain.txid(raw);
  const utxos = c.coins.map((value, vout) => ({ txid: prevTxid, vout, value: String(value), script: chain.hex(fromScript), confirmations: 1 }));
  const built = await chain.buildPayment({
    key: from.key, utxos, getRawTx: async () => chain.hex(raw),
    script: c.to, amount: c.amount, data: c.data, feePerByte: c.feePerByte,
  });
  payments.push({
    name: c.name,
    fromLabel: from.label,
    prevTx: chain.hex(raw),
    utxos,
    toScript: chain.hex(c.to),
    amount: String(c.amount),
    data: c.data ? chain.hex(c.data) : '',
    tx: built.hex,
    txid: built.txid,
    fee: String(built.fee),
    ...(c.feePerByte ? { feePerByte: String(c.feePerByte) } : {}),
  });
}

console.log(JSON.stringify({
  note: 'Generated by cmd/dogevm/testdata/wallet_vectors.mjs from the web wallet. Keys are sha256 of the labels; nothing here is secret.',
  versions,
  keys: keys.map((k) => k.vector),
  deposit,
  reserveScript: chain.hex(chain.pkScript(reserve)),
  payments,
}, null, 2));
