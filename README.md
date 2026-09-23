# DogecoinVM

A Dogecoin virtual machine for [Metal Blockchain](https://github.com/MetalBlockchain/metalgo): a UTXO ledger with Dogecoin addresses, keys and script, run under Snowman consensus instead of proof of work.

DogecoinVM is a fork of [MetalBlockchain/btcvm](https://github.com/MetalBlockchain/btcvm), which embeds [btcd](https://github.com/btcsuite/btcd) as the ledger and script engine. It is a second ledger for existing DOGE, not a new coin:

- **No premine and no block reward.** The genesis blocks pay nothing, and the coinbase can only claim transaction fees. DOGE enters the ledger only through a two-way peg with Dogecoin ([docs/BRIDGE.md](docs/BRIDGE.md)).
- **Same addresses and keys as Dogecoin.** A `D…` address, WIF key or `dgpv`/`dgub` extended key is the same on both chains.
- **No change to Dogecoin itself.** Dogecoin keeps running exactly as it does today.

> **Status: beta.** The code has not been audited. Keep amounts small.

**Live:** **https://metaldoge.com** runs DogecoinVM as an L1 on Metal mainnet, pegged to Dogecoin mainnet, as a capped beta with real DOGE (deposits up to 100 DOGE, at most 1,000 DOGE circulating). JSON-RPC is at `/rpc`. [docs/TESTING.md](docs/TESTING.md) lists its IDs and covers running the whole stack locally.

## Networks

| | Mainnet | Testnet (default) |
|---|---|---|
| Select with | `--mainnet` / `"mainNet": true` | `--testnet` / `"testNet": true` |
| P2PKH / P2SH / WIF version | 30 / 22 / 158 | 113 / 196 / 241 |
| Extended keys | `dgpv` / `dgub` | `tprv` / `tpub` |
| BIP44 coin type | 3 | 1 |
| Max single output | 10,000,000,000 DOGE (Dogecoin's `MAX_MONEY`) | same |

The parameters live in [`btcd/params.go`](btcd/params.go). The encodings match Dogecoin Core's `chainparams.cpp`.

## Fees and policy

Relay policy matches Dogecoin Core 1.14 ([`btcd/mempool/policy.go`](btcd/mempool/policy.go)):

| Rule | Value |
|---|---|
| Minimum relay fee | 0.001 DOGE/kB, required on every transaction (no free or priority relay) |
| Soft dust | each spendable output below 0.01 DOGE adds 0.01 DOGE to the required fee |
| Hard dust | a spendable output below 0.001 DOGE makes the transaction non-standard |
| OP_RETURN | never dust; at most one per transaction |
| Replace-by-fee increment | 0.0001 DOGE/kB |

Dogecoin has no SegWit or Taproot, and DogecoinVM never activates either. Witness outputs are non-standard, and a block carrying witness data is invalid.

## How it works

Metal's Snowman consensus orders blocks, and btcd validates and stores them. The adapter in [`vm/block_adapter.go`](vm/block_adapter.go) keeps one rule: **btcd only ever holds accepted blocks.**

- `ParseBlock` decodes a block without storing it, and rejects any encoding other than the block's canonical one.
- `Verify` runs full consensus validation against the last accepted block, read-only, via btcd's `CheckConnectBlockTemplate`.
- `Accept` is the only place a block is written to btcd.
- `Reject` has nothing to undo.

As a result, btcd's chain tip is always the last accepted block. Blocks propagate through Snowman, not gossip; only transactions are gossiped. Proof-of-work checks are disabled, since Snowman provides the security.

## Two-way peg

A peg reserve, created by consensus in the chain's first blocks and locked to an m-of-n signer multisig, backs every DOGE on DogecoinVM. The `dogevm` CLI ([`cmd/dogevm`](cmd/dogevm)) is a wallet and the bridge:

- **Peg-in:** a Dogecoin deposit to the peg address is credited from the reserve.
- **Peg-out:** DOGE paid back into the reserve is released on Dogecoin.
- **Audit:** `dogevm audit` checks that DOGE locked on Dogecoin covers everything circulating on DogecoinVM.

The peg is federated: the signers are trusted. See [docs/BRIDGE.md](docs/BRIDGE.md) for the design, message format and trust model.

The signers can run separately, each with one key on its own machine, checking every transaction against its own view of both chains before signing. A compromised bridge process can then delay transfers but can't move locked DOGE. See [docs/SIGNERS.md](docs/SIGNERS.md).

## Status and roadmap

The peg is proven end to end on mainnet. On 23 September 2026 the bridge
completed its first round trip:

| Step | Chain | Transaction |
| --- | --- | --- |
| Deposit, 1 DOGE | Dogecoin | [`74e053f6…19ea11b9`](https://blockchair.com/dogecoin/transaction/74e053f6b3c1bd3a9947dddbc42cf54dd9761f7d7cdc6a1ea30947b119ea11b9) |
| Credit, 0.99 DOGE | DogecoinVM | [`39a6c47e…1d8475`](https://metaldoge.com/explorer#/tx/39a6c47ee4ee60c68f19b963c40911730663498c78aff055a92576d1317d8475) |
| Withdrawal, 5 DOGE | DogecoinVM | [`4a447aff…6c3852`](https://metaldoge.com/explorer#/tx/4a447affd54af5dca7feb45195e3e479bd7a265fb078b31d11f4d218d66c3852) |
| Payout, 4.9 DOGE | Dogecoin | [`8be21486…ec9737`](https://blockchair.com/dogecoin/transaction/8be21486441207524403742b3c0c9023b5764fb4e1f12ef0e9f493d19aec9737) |

Next is removing single points of failure: moving the bridge onto separate
signers, bringing in independent operators, and more validators. Then an
external audit, and later crediting deposits on Dogecoin proofs rather than
signatures. The full roadmap is at
[metaldoge.com/roadmap](https://metaldoge.com/roadmap).

## Building and testing

Requires Go 1.24+.

```bash
go build ./...
go test ./vm/ ./cmd/... ./btcd/ ./btcd/mempool/
```

The VM targets metalgo v1.13.5 (rpcchainvm protocol 43). Its VM ID is `mEUwHwfd8UTHf23UYkQxHvy1n1EGwWieXQnjmtzSryJRZckzu` (from the name `dogecoinvm`).

Some vendored btcd tests fail the same way on upstream btcvm, because proof of work is disabled there. Examples are `TestFullBlocks` and `TestUtxoCacheFlush`.

See [`docs/README.md`](docs/README.md) for the Makefile targets that build the plugin and run a local five-node network with `metal-network-runner`.

## License

See [LICENSE](LICENSE) and [NOTICE](NOTICE). The vendored btcd is under its own ISC license in [`btcd/LICENSE`](btcd/LICENSE).
