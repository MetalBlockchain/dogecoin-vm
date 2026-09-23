// Runs the web wallet's chain.js on inputs from TestWebChainMatchesGo and
// prints what it computes, for the Go test to check.
import { readFileSync } from 'node:fs';
import * as chain from '../web/chain.js';

const input = JSON.parse(readFileSync(process.argv[2], 'utf8'));
const key = chain.unhex(input.keyHex);
const dest = chain.keyDestination(key);

const payment = await chain.buildPayment({
  key,
  utxos: [input.utxo],
  script: chain.unhex(input.toScript),
  amount: BigInt(input.amount),
  data: input.data ? chain.unhex(input.data) : undefined,
});

console.log(JSON.stringify({
  vmAddress: chain.encodeAddress(dest, input.vmVersions),
  dogeAddress: chain.encodeAddress(dest, input.dogeVersions),
  vmWIF: chain.wif(key, input.vmVersions),
  dogeWIF: chain.wif(key, input.dogeVersions),
  keyFromWIF: chain.hex(chain.parseKey(chain.wif(key, input.dogeVersions))),
  depositAddress: chain.depositAddress(dest, input.signers, input.dogeVersions),
  txHex: payment.hex,
}));
