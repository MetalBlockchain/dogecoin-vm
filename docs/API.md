# DogecoinVM API reference

The DogecoinVM site at **https://metaldoge.com** serves two APIs, the same ones
its web wallet and the Mac app use:

- **`/api/*`**: a JSON API for wallets and explorers: balances, transactions,
  the bridge (deposits and withdrawals), proof of reserves, and the Dogecoin
  side of a wallet.
- **`/rpc`**: the DogecoinVM node's own JSON-RPC (btcd / Bitcoin Core style),
  through a limited public login.

> **Alpha.** DogecoinVM and its bridge are alpha software, not yet audited, and
> capped. Read the [alpha terms](https://metaldoge.com/terms) before moving
> real DOGE. The current caps are always in [`GET /api/info`](#get-apiinfo).

**Keys never go to any server.** Nothing in this API takes a private key. Make
keys and sign transactions on the user's own device (see
[Build a wallet in 5 steps](#build-a-wallet-in-5-steps) and
[Security](#security)).

## Conventions

- **Amounts in DOGE** are decimal strings with 8 places, such as `"1.50000000"`.
- **Amounts in koinu** (1 DOGE = 100,000,000 koinu) are integer strings, used
  where a wallet signs: each UTXO's `value`. Strings, so JavaScript never
  rounds them.
- **Addresses** are base58 Dogecoin mainnet addresses (`D…` for P2PKH, `9…`/`A…`
  for P2SH). The same string is a valid address on Dogecoin and on DogecoinVM;
  each endpoint says which chain it means.
- **Errors** are JSON: `{"error": "…"}`, with HTTP status 400 (bad request),
  404 (not found), 429 (rate limited), 502 (node didn't answer) or 503 (busy or
  starting up).
- **Confirmations** on DogecoinVM: a transaction in a block is final (Snowman
  consensus, no reorganisations); `confirmations: 0` means it is still in the
  mempool. On Dogecoin, confirmations mean what they always do.
- **Times** are Unix seconds unless the field says otherwise.

In the examples, `D5EQRCnPMXmRvUoZwC7gu7fYspean3PQ9a` is a made-up address and
the transaction IDs are placeholders. The response shapes are real.

## Limits

| What | Limit |
|---|---|
| Explorer-style endpoints (`/api/address`, `/api/tx`, `/api/block`, `/api/blocks`, `/api/doge/import`, `/api/doge/rawtx`) | 8 at a time across all users; a request waiting over 10 s gets `503 busy; try again shortly` |
| New deposit addresses (`POST /api/deposit-address`) and new Dogecoin addresses (`POST /api/doge/watch`) | 30 per hour per IP (registering one already known is free) |
| `/api/events` streams | 5,000 open at once |
| Request bodies | 1 MiB |

This is one public site, not a hosted API service: there are no API keys or
uptime guarantees. An app with real traffic should run its own node
([RUN-A-NODE.md](RUN-A-NODE.md)) and use its JSON-RPC.

---

## Network and bridge

### `GET /api/info`

The network's parameters and the bridge's current policy. Read it at start-up
and **check `chainID` and `dogecoinvmNetwork`** before signing anything.

```sh
curl https://metaldoge.com/api/info
```

```json
{
  "chainID": "2hFCfzdMmfXBxYgvvdL7BYiJAxdejyn4AksMYUM2eM5gN7Xrjy",
  "dogecoinvmNetwork": "dogecoinvm",
  "dogecoinNetwork": "mainnet",
  "dogecoinvmVersions": { "p2pkh": 30, "p2sh": 22, "wif": 158 },
  "dogecoinVersions": { "p2pkh": 30, "p2sh": 22, "wif": 158 },
  "pegAddress": "AAvNfukpAa4iTcRJPetxuxX8XxbC5gFqUM",
  "reserveAddress": "AAvNfukpAa4iTcRJPetxuxX8XxbC5gFqUM",
  "signers": { "required": 2, "publicKeys": ["0212a0…", "…", "…"] },
  "depositConfirmations": 20,
  "confirmationTiers": [
    { "upTo": "10.00000000", "confirmations": 6 },
    { "upTo": "50.00000000", "confirmations": 12 }
  ],
  "vmFee": "0.01000000",
  "dogeFee": "0.10000000",
  "minDeposit": "1.00000000",
  "maxDeposit": "100.00000000",
  "minPegOut": "2.00000000",
  "maxCirculating": "4200.00000000",
  "faucet": { "enabled": false },
  "dogeWallet": true,
  "bridgeVersion": "0.2.0"
}
```

| Field | Meaning |
|---|---|
| `chainID` | DogecoinVM's blockchain ID on Metal. Pin it in your app. |
| `*Versions` | Base58 version bytes. Identical on both chains: that's why one key has one address on both. |
| `pegAddress` | The bridge's multisig on **Dogecoin** (the shared deposit address; see [deposits](#post-apideposit-address)). |
| `reserveAddress` | The bridge's reserve on **DogecoinVM**. Paying DOGE into it (with a tag) is how you withdraw. It is the same script as `pegAddress`, so the same string. |
| `signers` | The bridge's m-of-n signer set (public keys). |
| `confirmationTiers`, `depositConfirmations` | Dogecoin confirmations a deposit needs before it is credited: by amount, up to each `upTo`; above the last tier, `depositConfirmations`. The tiers already include the mainnet minimum: no mainnet deposit is credited before 6 confirmations. |
| `vmFee` | What the bridge deducts from a deposit when it credits it on DogecoinVM. |
| `dogeFee` | What the bridge deducts from a withdrawal for the Dogecoin payout. |
| `minDeposit` / `maxDeposit` | Per-deposit limits. A deposit outside them isn't credited; it's held for a refund. |
| `minPegOut` | The smallest withdrawal. |
| `maxCirculating` | The alpha cap on all DOGE on DogecoinVM. A deposit that would exceed it waits. |
| `faucet` | Test networks only; disabled on mainnet. |
| `dogeWallet` | Whether the `/api/doge/*` endpoints are served. |
| `bridgeVersion` | The bridge software's release, such as `0.2.0`. |

### `GET /api/status`

Both chains' heights, Dogecoin sync, the peg audit (proof of reserves), measured
finality, and whether the bridge is paused. Updated every 15 s.

```sh
curl https://metaldoge.com/api/status
```

```json
{
  "dogecoinvmHeight": 51,
  "dogecoinHeight": 6400367,
  "dogecoinSync": { "headers": 6400367, "progress": 0.9999997, "syncing": false, "available": true },
  "dogecoinBlockTime": 1791065270,
  "dogecoinSupply": { "amount": "156175782865", "height": 6400328 },
  "finality": { "payments": 21, "medianMs": 196, "p90Ms": 268, "latestMs": 97 },
  "audit": {
    "solvent": true,
    "locked": "46.61000000",
    "circulating": "46.61000000",
    "pendingPegIns": "0.00000000",
    "pendingPegOuts": "0.00000000",
    "surplus": "0.00000000",
    "unclaimedOnDogecoin": "0.00000000",
    "unclaimedOnDogecoinVM": "0.00000000"
  },
  "updated": "2026-10-03T22:08:11Z"
}
```

- `audit.locked` is DOGE held by the bridge on Dogecoin; `audit.circulating` is
  DOGE released onto DogecoinVM. `solvent` is true when locked covers everything
  owed.
- `finality` is measured live: from the moment this site receives a payment to
  the moment its block is accepted (final), over recent payments.
- `paused` (object) appears only while the bridge is paused; `error` appears in
  place of `audit` if the bridge can't read a chain.

### `GET /api/health`

Health checks, for uptime monitors: HTTP 200 when everything passes (including
`"status": "degraded"`, a deliberate pause or a Dogecoin node catching up), 503
when something is down.

```json
{
  "ok": true,
  "status": "ok",
  "checks": [
    { "name": "pause", "ok": true, "detail": "not paused" },
    { "name": "peg", "ok": true, "detail": "locked 46.61000000, circulating 46.61000000, …" },
    …
  ],
  "updated": "2026-10-03T22:08:11Z"
}
```

The checks are `pause`, `peg`, `bridge`, `dogecoin`, `wallet` (the bridge's
Dogecoin Core wallet), `dogecoinvm` and `validator` (its fee balance). If the
bridge can't account for Dogecoin's history, a failing `history` check takes
the place of `peg`.

### `GET /api/reserves`

Every output the bridge holds on Dogecoin, for checking the proof of reserves
yourself against any Dogecoin explorer.

```json
{
  "lockedOnDogecoin": "46.61000000",
  "circulating": "46.61000000",
  "pegAddress": "AAvNfukpAa4iTcRJPetxuxX8XxbC5gFqUM",
  "reserveAddress": "AAvNfukpAa4iTcRJPetxuxX8XxbC5gFqUM",
  "reserveCreated": "9000000000.00000000",
  "reserveHeld": "8999999953.39000000",
  "personalDepositCount": 11,
  "dogecoinOutputs": [
    { "address": "9rctRHEpjCvZmVJqwSaGNTzCqUWKS9MmYB", "amount": "1.00000000",
      "confirmations": 11580, "txid": "709b55bd…201b", "vout": 0 }
  ]
}
```

`reserveCreated` is the reserve the chain created at genesis (it backs nothing
by itself); `reserveCreated − reserveHeld` is what circulates. Anything locked
above what circulates and is owed (`audit.surplus` in `/api/status`) is the
bridge's own operating capital and kept change, which pays its housekeeping
fees.

### `GET /api/activity`

Recent bridge activity: deposits and withdrawals, newest first (up to 100).

```json
[
  { "type": "withdrawal", "status": "paid", "amount": "2.00000000", "pays": "1.90000000",
    "to": "D5EQRCnPMXmRvUoZwC7gu7fYspean3PQ9a",
    "dogecoinvmTxid": "27ca64c0…a1e3", "dogecoinTxid": "1f3cb18e…b7a9", "time": 1790899406 },
  { "type": "deposit", "status": "credited", "amount": "5.00000000", "credited": "4.99000000",
    "to": "D5EQRCnPMXmRvUoZwC7gu7fYspean3PQ9a",
    "dogecoinTxid": "959f7afe…b68b", "vout": 0, "confirmations": 6, "required": 6,
    "creditTxid": "709b55bd…201b", "time": 1790899100 }
]
```

- A deposit's `status` is `waiting` (confirming, or waiting to be credited),
  `held` (not credited; refundable), `credited` (with `creditTxid` and
  `credited`, the amount after `vmFee`) or `refunded` (with `refundTxid`).
  `to` is the DogecoinVM address it credits, when it names one.
- A withdrawal's `status` is `waiting` or `paid` (with the Dogecoin
  `dogecoinTxid`).

---

## DogecoinVM: addresses, transactions, blocks

### `GET /api/address/{address}`

A DogecoinVM address's balance, unspent outputs and last 50 transactions.

```sh
curl https://metaldoge.com/api/address/D5EQRCnPMXmRvUoZwC7gu7fYspean3PQ9a
```

```json
{
  "address": "D5EQRCnPMXmRvUoZwC7gu7fYspean3PQ9a",
  "confirmed": "5.65601300",
  "pending": "0.00000000",
  "utxos": [
    { "txid": "709b55bd…201b", "vout": 0, "value": "99000000",
      "script": "76a914…88ac", "confirmations": 3 }
  ],
  "history": [
    { "txid": "709b55bd…201b", "confirmations": 3, "net": "0.99000000", "time": 1790899408 }
  ]
}
```

- `utxos[].value` is in **koinu** (integer string); `confirmed`, `pending` and
  `history[].net` are in DOGE. `net` is signed: negative for a payment out.
- `pending` is outputs still in the mempool (normally for well under a second).
- **Don't trust `value` when signing**: fetch the raw transaction
  ([`/api/rawtx`](#get-apirawtxtxid)), check it hashes to `txid`, and read the
  output's value and script from it. Legacy signatures don't commit to input
  amounts, so a lying server could otherwise inflate your fee.

### `GET /api/tx/{txid}`

A DogecoinVM transaction, decoded and described.

```json
{
  "txid": "27ca64c0…a1e3",
  "confirmations": 1,
  "blockHash": "5af2392a…ee73",
  "time": 1790899408,
  "kind": "credit",
  "label": "Credit for a deposit on Dogecoin, released from the peg reserve",
  "inputs": [ { "address": "AAvNfukpAa4iTcRJPetxuxX8XxbC5gFqUM", "value": "8999999935.06000000", "note": "peg reserve" } ],
  "outputs": [
    { "address": "D5EQRCnPMXmRvUoZwC7gu7fYspean3PQ9a", "value": "0.99000000" },
    { "address": "AAvNfukpAa4iTcRJPetxuxX8XxbC5gFqUM", "value": "8999999934.06000000", "note": "peg reserve" },
    { "value": "0.00000000", "note": "bridge message" }
  ],
  "fee": "0.01000000",
  "dogecoinTxid": "959f7afe…b68b"
}
```

`kind` is one of `transfer`, `credit` (a bridge deposit credited), `withdrawal`
(a payment into the reserve), `maintenance` (the bridge moving the reserve to
itself: a `DVMZ` fence, as in the 11 October 2026 clean start, or a `DVMX`
cancel before a refund; its fee is the bridge's own), `reserve`, `reward`. `dogecoinTxid` links a
bridge transaction to its Dogecoin side.

### `GET /api/rawtx/{txid}`

A DogecoinVM transaction's raw hex: `{"hex": "0100…"}`. 404 if the chain doesn't
have it.

### `POST /api/tx`

Broadcast a signed DogecoinVM transaction.

```sh
curl -X POST https://metaldoge.com/api/tx -H 'content-type: application/json' \
  -d '{"hex":"0100000001…"}'
```

```json
{ "txid": "709b55bd…201b" }
```

- **400** `rejected by the network: …`: the node refused it; it was not sent.
- **502**: the node didn't answer, so it **may or may not** have been sent.
  Keep your record and check the txid (`/api/tx/{txid}`) before sending again.

### `GET /api/blocks?before={height}`

The 15 newest blocks (or the 15 below `before`): `{"tip": 51, "blocks": [{height, hash, time, txCount, previousHash}, …]}`.
DogecoinVM makes a block only when there are transactions, so heights grow
slowly and a block can be minutes or days after the last.

### `GET /api/block/{height-or-hash}`

One block with its transactions (up to 50 described):
`{"block": {height, hash, time, txCount, previousHash, txids}, "transactions": [tx, …], "txCount": n}`.

### `GET /api/events`

A Server-Sent Events stream that says when a chain has a new block, so wallets
refresh at once instead of polling. It carries no account data.

```
event: block
data: {"chain":"dogecoinvm","height":52}

event: block
data: {"chain":"dogecoin","height":6400368}
```

On connect, the latest height of each chain is sent first.

---

## The bridge: deposits and withdrawals

### `POST /api/deposit-address`

Gives the **personal Dogecoin deposit address** for a DogecoinVM address. DOGE
sent to it on Dogecoin is credited to that DogecoinVM address (less `vmFee`)
once it has enough confirmations. Each DogecoinVM address has one deposit
address, and it never changes; registering it again returns the same one.

```sh
curl -X POST https://metaldoge.com/api/deposit-address -H 'content-type: application/json' \
  -d '{"address":"D5EQRCnPMXmRvUoZwC7gu7fYspean3PQ9a"}'
```

```json
{
  "depositAddress": "9rctRHEpjCvZmVJqwSaGNTzCqUWKS9MmYB",
  "creditTo": "D5EQRCnPMXmRvUoZwC7gu7fYspean3PQ9a",
  "redeemScript": "15000101…0101755221…53ae"
}
```

**Check it yourself; don't just trust the server.** The deposit address is the
P2SH of `redeemScript`, which is

```
OP_DATA_21 <type> <hash160 of the DogecoinVM address>  OP_DROP  <the bridge's m-of-n multisig>
```

where `type` is `0x00` for P2PKH and `0x01` for P2SH, and the multisig is
`OP_m <signers.publicKeys…> OP_n OP_CHECKMULTISIG` from `/api/info`. A wallet
should rebuild the script from `/api/info` and the user's own address, and
refuse to show a deposit address that doesn't match.

Rules (alpha; read the current values from `/api/info`):
- A deposit below `minDeposit` or above `maxDeposit` isn't credited: it's
  **held** and can be refunded on request ([terms](https://metaldoge.com/terms)).
- A deposit that would take circulating DOGE above `maxCirculating` waits for room.
- Credit comes after the confirmations for its amount (`confirmationTiers`).
- Send from any Dogecoin wallet. Several deposits to the same address are fine.

Advanced: instead of a personal address, a deposit can pay `pegAddress` with an
`OP_RETURN` of `DVMD <type> <hash160>` (25 bytes) naming the DogecoinVM
address. A payment to `pegAddress` without that tag has no destination and is
held. Don't use the `DVMC` tag: it marks the operator's own contributions to
the peg, which are never credited or refunded.

### `GET /api/deposits/{dogecoinvm-address}`

Deposits made for a DogecoinVM address, and where each one is.

```json
[
  { "txid": "959f7afe…b68b", "vout": 0, "amount": "1.00000000",
    "confirmations": 6, "required": 6, "status": "credited",
    "creditTxid": "27ca64c0…a1e3", "credited": "0.99000000" },
  { "txid": "1f3cb18e…b7a9", "vout": 1, "amount": "150.00000000",
    "confirmations": 40, "required": 20, "status": "held",
    "reason": "above the maximum deposit" }
]
```

`status` is `confirming`, `crediting` (enough confirmations; credited within a
few seconds), `waiting_for_capacity`, `credited`, `held` (with a `reason`) or
`refunded` (with `refundTxid`). `crediting` can last longer when many deposits
arrive together: the bridge limits how much it credits before a deposit has
the full `depositConfirmations`, and the rest wait for more confirmations.

### Withdrawing: a payment into the reserve

A withdrawal is an ordinary DogecoinVM transaction that pays DOGE into
`reserveAddress` and names the Dogecoin address to pay, in its **only**
`OP_RETURN` output:

| Output | Script |
|---|---|
| Payment | the amount (at least `minPegOut`) to `reserveAddress` (P2SH) |
| Tag | `OP_RETURN` pushing 25 bytes: `"DVMO"` (ASCII) ‖ `type` (1 byte: `0x00` P2PKH, `0x01` P2SH) ‖ the 20-byte hash160 of the **Dogecoin** destination address |
| Change | back to your own address, as usual |

The bridge pays the Dogecoin address `amount − dogeFee`. (In rare cases up to
0.01 DOGE less, to keep the peg's change above Dogecoin's dust limit.) Rules:
- Exactly one `OP_RETURN` output; with none, or two, the payment isn't a
  withdrawal and isn't paid out (it's held as unclaimed on DogecoinVM).
- Below `minPegOut`, it isn't paid out either.
- The Dogecoin destination can't be the bridge's own `pegAddress`.
- The payout goes out once the payment is final on DogecoinVM (normally
  within seconds), and arrives with Dogecoin's usual block times. Payouts go
  one at a time: while one waits for its Dogecoin block, the next waits for
  it, usually a minute or two.

Example tag for withdrawing to the made-up `D5R2B6TsTmVihS3WLQMwbpfSsCV9WeqE4v`
(hash160 `0303…03`):

```
6a 19 44564d4f 00 0303030303030303030303030303030303030303
OP_RETURN, push 25: "DVMO", P2PKH, hash160
```

### `GET /api/pegout/{dogecoinvm-txid}`

A withdrawal's progress, by the txid of the DogecoinVM payment.

```json
{ "status": "paid", "amount": "2.00000000", "pays": "1.90000000",
  "to": "D5R2B6TsTmVihS3WLQMwbpfSsCV9WeqE4v",
  "paymentTxid": "1f3cb18e…b7a9", "paymentConfirmations": 4 }
```

`status` is `pending` (being paid), `paid` (with the Dogecoin `paymentTxid` and
its confirmations), or `unknown` (not final on DogecoinVM yet, or not a valid
withdrawal).

---

## The Dogecoin side of a wallet

The site also serves a user's **Dogecoin** (mainnet) balance, so a light wallet
needs no Dogecoin node. Addresses must be registered first.

### `POST /api/doge/watch`

Start indexing a Dogecoin address: `{"address": "D…"}` →
`{"address": "D…", "watchedFrom": 6400000}`. Payments are found from
`watchedFrom` onwards; import older ones with `/api/doge/import`.

### `GET /api/doge/address/{address}`

```json
{
  "address": "D5EQRCnPMXmRvUoZwC7gu7fYspean3PQ9a",
  "confirmed": "5.00000000",
  "pending": "0.00000000",
  "utxos": [ { "txid": "1f3cb18e…b7a9", "vout": 0, "value": "500000000",
               "script": "76a914…88ac", "confirmations": 6 } ],
  "history": [ { "txid": "1f3cb18e…b7a9", "net": "5.00000000", "confirmations": 6, "time": 1791008000 } ],
  "watchedFrom": 6400000,
  "indexedTo": 6400367
}
```

404 `address is not registered` until `/api/doge/watch`; 503 while the site's
Dogecoin node is catching up.

### `POST /api/doge/import`

Add a payment made before the address was registered:
`{"address": "D…", "txid": "…"}` → `{"imported": 1}`.

### `GET /api/doge/rawtx/{txid}` and `POST /api/doge/tx`

The Dogecoin equivalents of `/api/rawtx` and `/api/tx`: fetch a Dogecoin
transaction's hex (to check what you spend), and broadcast a signed Dogecoin
transaction (`{"hex": …}` → `{"txid": …}`, same 400/502 rules).

---

## JSON-RPC: `/rpc`

The DogecoinVM node's own JSON-RPC (btcd, Bitcoin Core style), through a
**limited** login whose username and password are both `public`:

```sh
curl -s -u public:public https://metaldoge.com/rpc -H 'content-type: application/json' \
  -d '{"jsonrpc":"1.0","id":1,"method":"getblockcount","params":[]}'
```

```json
{"result": 51, "error": null, "id": 1}
```

The limited login may call: `getbestblock`, `getbestblockhash`, `getblock`,
`getblockcount`, `getblockhash`, `getblockheader`, `getchaintips`, `getheaders`,
`getcfilter`, `getcfilterheader`, `getinfo`, `getrawmempool`,
`getrawtransaction`, `gettxout`, `searchrawtransactions`, `decoderawtransaction`,
`decodescript`, `createrawtransaction`, `estimatefee`, `validateaddress`,
`verifymessage`, `getcurrentnet`, `getdifficulty`, `getnettotals`, `uptime`,
`version` and `help` (and `notifyblocks` over the WebSocket). Wallet,
node-administration and broadcast methods are refused: to broadcast, use
`POST /api/tx`.

`searchrawtransactions` (the node keeps an address index) returns at most 500
transactions per call; read longer histories in pages with `skip`.

---

## Build a wallet in 5 steps

1. **Make the key on the device.** Use any Dogecoin library (for example
   libdogecoin, or a Bitcoin library with Dogecoin's mainnet parameters:
   P2PKH 30, P2SH 22, WIF 158, BIP44 coin type 3). A compressed secp256k1 key
   gives one `D…` address, valid on **both** Dogecoin and DogecoinVM. Back the
   key up; nobody can recover it.
2. **Check the network.** `GET /api/info`: refuse to continue unless `chainID`
   is `2hFCfzdMmfXBxYgvvdL7BYiJAxdejyn4AksMYUM2eM5gN7Xrjy` and
   `dogecoinvmNetwork` is `dogecoinvm`.
3. **Read the balance.** `GET /api/address/{address}` for DogecoinVM (and
   `/api/doge/watch` + `/api/doge/address` for Dogecoin). For each UTXO you
   will spend, `GET /api/rawtx/{txid}`, check the hex hashes to `txid`, and take
   the value and script from that output.
4. **Build and sign** a standard legacy transaction: version 1, P2PKH inputs
   signed with `SIGHASH_ALL`, low-S DER signatures, compressed public keys.
   DogecoinVM's relay rules are Dogecoin Core 1.14's:
   - fee at least **0.001 DOGE per kB** (100 koinu per byte) of the signed size;
   - each spendable output below 0.01 DOGE adds 0.01 DOGE to the required fee;
     an output below 0.001 DOGE is non-standard;
   - at most one `OP_RETURN`; no SegWit.

   (`vmFee` and `dogeFee` in `/api/info` are the *bridge's* charges on credits
   and payouts, not network fees.)
5. **Broadcast** with `POST /api/tx`. Final
   in under a second; `/api/events` tells you when the block lands.

**Bridge in:** `POST /api/deposit-address` with the user's DogecoinVM address,
check the redeem script as above, and show the deposit address (it's a Dogecoin
address: say so). Track it with `/api/deposits/{address}`.
**Bridge out:** pay at least `minPegOut` to `reserveAddress` with the `DVMO` tag
naming the Dogecoin address; track it with `/api/pegout/{txid}`.

## Payment links

DIP 21 (draft) proposes [`dogecoin:` and `dogecoinvm:` payment
links](https://github.com/MetalBlockchain/dogecoin-vm/blob/dogecoin/docs/dip-0021.md),
so a link or QR code can say which chain a payment is for. No wallet supports
it yet.

## Security

- **Never send a private key, seed or WIF to any server**, ours included.
  Nothing here needs one.
- **Pin the chain.** Check `chainID` (and the network names) from `/api/info`
  before signing, and keep the expected value in your app rather than trusting
  whatever a server returns.
- **Dogecoin and DogecoinVM use the same addresses.** A `D…` address doesn't
  say which chain it's on, so:
  - label every balance, address field and confirmation screen with its chain;
  - never send DOGE on Dogecoin to a DogecoinVM address expecting it to bridge:
    it lands on Dogecoin, at the same key (recoverable with that key, but not
    bridged). Only the **deposit address** bridges;
  - keep the two chains' UTXOs apart, and fetch each from its own endpoints, so
    a server can't present one chain's coins as the other's;
  - for a withdrawal, show the Dogecoin address decoded from your own `DVMO`
    tag before signing.
- **Verify what a server tells you:** input values from raw transactions,
  deposit addresses from the redeem script, withdrawal destinations from the tag.
- **Alpha:** all three signer keys are held by one operator during the alpha,
  and amounts are capped. See the [terms](https://metaldoge.com/terms) and the
  [roadmap](https://metaldoge.com/roadmap).
- Report vulnerabilities privately via
  [GitHub security advisories](https://github.com/MetalBlockchain/dogecoin-vm/security/advisories/new).

<!-- TODO: once the security advisories are published, add: "Past advisories
     are [published on GitHub](https://github.com/MetalBlockchain/dogecoin-vm/security/advisories)." -->
