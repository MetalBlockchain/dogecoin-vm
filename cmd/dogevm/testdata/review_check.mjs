// Checks the web wallet's review step (TestWebReview): a planned payment's
// outputs decode to what was asked for, the fee and amounts add up, a
// withdrawal's Dogecoin address reads back from its tag, and signing
// refuses a plan whose outputs changed after review. Prints "ok".
//
// Test keys are sha256 of public labels: this file holds no secret.
import assert from 'node:assert/strict';
import * as chain from '../web/chain.js';
import { sha256 } from '../web/vendor/noble-hashes-1.8.0/sha2.js';

const versions = { p2pkh: 30, p2sh: 22, wif: 158 };
const key = sha256(new TextEncoder().encode('dogevm review key'));
const other = chain.keyDestination(sha256(new TextEncoder().encode('dogevm review payee')));
const me = chain.keyDestination(key);
const myScript = chain.pkScript(me);

function u32(n) { const b = new Uint8Array(4); new DataView(b.buffer).setUint32(0, n, true); return b; }
function u64(n) { const b = new Uint8Array(8); new DataView(b.buffer).setBigUint64(0, BigInt(n), true); return b; }
function cat(...parts) { const o = new Uint8Array(parts.reduce((n, p) => n + p.length, 0)); let i = 0; for (const p of parts) { o.set(p, i); i += p.length; } return o; }
const prev = cat(u32(1), Uint8Array.of(1), new Uint8Array(32).fill(7), u32(0), Uint8Array.of(1, 0x51), u32(0xffffffff),
  Uint8Array.of(1), u64(50n * chain.KOINU), Uint8Array.of(myScript.length), myScript, u32(0));
const prevId = chain.txid(prev);
const utxos = [{ txid: prevId, vout: 0, value: String(50n * chain.KOINU), confirmations: 3, script: chain.hex(myScript) }];
const getRawTx = async () => chain.hex(prev);

// A withdrawal: pay 10 DOGE with a DVMO tag naming a Dogecoin address.
const amount = 10n * chain.KOINU;
const plan = await chain.planPayment({
  key, utxos, getRawTx, script: chain.pkScript(other), amount, data: chain.pegOutData(other),
});
const outs = chain.reviewOutputs(plan.unsignedHex, versions);
assert.equal(outs.length, 3);
assert.equal(outs[0].address, chain.encodeAddress(other, versions));
assert.equal(outs[0].value, amount);
assert.equal(chain.pegOutDestination(outs[1].data, versions), chain.encodeAddress(other, versions));
assert.equal(outs[2].address, chain.encodeAddress(me, versions)); // change
assert.equal(outs.reduce((n, o) => n + o.value, 0n) + plan.fee, plan.inputTotal);
assert.equal(chain.pegOutDestination(new Uint8Array(25), versions), null);

// Signing pays exactly what was reviewed, and matches buildPayment.
const signed = await chain.signPlan(plan, key);
const direct = await chain.buildPayment({ key, utxos, getRawTx, script: chain.pkScript(other), amount, data: chain.pegOutData(other) });
assert.equal(signed.hex, direct.hex);
assert.equal(chain.reviewOutputs(signed.hex, versions).map((o) => o.address).join(), outs.map((o) => o.address).join());

// A plan whose outputs changed after review is refused.
const tampered = { ...plan, tx: { ...plan.tx, outputs: plan.tx.outputs.map((o, i) => (i === 0 ? { ...o, value: o.value + 1n } : o)) } };
await assert.rejects(chain.signPlan(tampered, key), /differs from the one reviewed/);

console.log('ok');
