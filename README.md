# DogecoinVM

A Dogecoin virtual machine for [Metal Blockchain](https://github.com/MetalBlockchain/metalgo): a UTXO ledger with Dogecoin addresses, keys and script, run under Snowman consensus instead of proof of work.

DogecoinVM is a fork of [MetalBlockchain/btcvm](https://github.com/MetalBlockchain/btcvm), which embeds [btcd](https://github.com/btcsuite/btcd) as the ledger and script engine. It is a second ledger for existing DOGE, not a new coin:

- **No premine and no block reward.** The genesis blocks pay nothing, and the coinbase can only claim transaction fees. DOGE is meant to enter the ledger by being locked on Dogecoin (see the roadmap).
- **Same addresses and keys as Dogecoin.** A `D…` address, WIF key or `dgpv`/`dgub` extended key is the same on both chains.
- **No change to Dogecoin itself.** Dogecoin keeps running exactly as it does today.

> **Status: early development, not ready for use.** Do not send real funds to anything built from this repository.

## Networks

| | Mainnet | Testnet (default) |
|---|---|---|
| Select with | `--mainnet` / `"mainNet": true` | `--testnet` / `"testNet": true` |
| P2PKH / P2SH / WIF version | 30 / 22 / 158 | 113 / 196 / 241 |
| Extended keys | `dgpv` / `dgub` | `tprv` / `tpub` |
| BIP44 coin type | 3 | 1 |
| Max single output | 10,000,000,000 DOGE (Dogecoin's `MAX_MONEY`) | same |

The parameters live in [`btcd/params.go`](btcd/params.go). The encodings match Dogecoin Core's `chainparams.cpp`.

## How it works

Metal's Snowman consensus orders blocks, and btcd validates and stores them. The adapter in [`vm/block_adapter.go`](vm/block_adapter.go) keeps one rule: **btcd only ever holds accepted blocks.**

- `ParseBlock` decodes a block without storing it, and rejects any encoding other than the block's canonical one.
- `Verify` runs full consensus validation against the last accepted block, read-only, via btcd's `CheckConnectBlockTemplate`.
- `Accept` is the only place a block is written to btcd.
- `Reject` has nothing to undo.

As a result, btcd's chain tip is always the last accepted block. Blocks propagate through Snowman, not gossip; only transactions are gossiped. Proof-of-work checks are disabled, since Snowman provides the security.

## Roadmap

1. ~~Dogecoin chain parameters~~ — done
2. ~~Snowman block lifecycle~~ — done
3. Dogecoin fee and dust policy; disable SegWit and Taproot — in progress
4. Peg-in/peg-out, supply accounting in arbitrary precision, and Warp/ICM messaging
5. RPC responses shaped like Dogecoin Core's

## Building and testing

Requires Go 1.24+.

```bash
go build ./vm/... ./cmd/... ./btcd/...
go test ./vm/ ./btcd/            # DogecoinVM lifecycle and parameter tests
```

Some vendored btcd tests fail the same way on upstream btcvm, because proof of work is disabled there. Examples are `TestFullBlocks` and `TestUtxoCacheFlush`. The root package's `factory.go` doesn't build against metalgo v1.12.2 either, which is also inherited from upstream.

See [`docs/README.md`](docs/README.md) for the Makefile targets that build the plugin and run a local five-node network with `metal-network-runner`.

## License

See [LICENSE](LICENSE) and [NOTICE](NOTICE). The vendored btcd is under its own ISC license in [`btcd/LICENSE`](btcd/LICENSE).
