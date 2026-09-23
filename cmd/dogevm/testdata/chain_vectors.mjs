// Runs the web wallet's chain.js on inputs from TestWebChainMatchesGo and
// prints what it computes, for the Go test to check.
import { readFileSync } from 'node:fs';
import * as chain from '../web/chain.js';

const input = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const key = chain.unhex(input.keyHex);
const dest = chain.keyDestination(key);

const pay = (utxo, rawTxs) => chain.buildPayment({
  key,
  utxos: [utxo],
  getRawTx: async (txid) => rawTxs[txid],
  script: chain.unhex(input.toScript),
  amount: BigInt(input.amount),
  data: input.data ? chain.unhex(input.data) : undefined,
});
const payment = await pay(input.utxo, input.rawTxs);

// A server that overstates the UTXO's value must not change what is signed:
// the value comes from the verified previous transaction.
const inflated = await pay({ ...input.utxo, value: String(BigInt(input.utxo.value) * 10n) }, input.rawTxs);

// A server that sends a different transaction for the txid is refused.
const refused = await pay(input.utxo, { [input.utxo.txid]: input.otherTx })
  .then(() => '', (err) => err.message);

// An uncompressed-key WIF is refused.
const uncompressed = (() => {
  try { chain.parseKey(input.uncompressedWIF); return ''; } catch (err) { return err.message; }
})();

console.log(JSON.stringify({
  vmAddress: chain.encodeAddress(dest, input.vmVersions),
  dogeAddress: chain.encodeAddress(dest, input.dogeVersions),
  vmWIF: chain.wif(key, input.vmVersions),
  dogeWIF: chain.wif(key, input.dogeVersions),
  keyFromWIF: chain.hex(chain.parseKey(chain.wif(key, input.dogeVersions))),
  depositAddress: chain.depositAddress(dest, input.signers, input.dogeVersions),
  txHex: payment.hex,
  txid: payment.txid,
  inflatedHex: inflated.hex,
  refused,
  uncompressed,
}));
