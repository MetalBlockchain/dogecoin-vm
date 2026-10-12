# DogecoinVM documentation

DogecoinVM is a Dogecoin-compatible virtual machine running on Metal Blockchain. It embeds btcd, configured with Dogecoin's parameters, under Metal's Snowman consensus. Blocks are built only when there are transactions to include.

| Page | What it covers |
| --- | --- |
| [TESTING.md](TESTING.md) | The live mainnet alpha's IDs, and running the whole stack on your own machine with `scripts/devnet.sh` |
| [BRIDGE.md](BRIDGE.md) | The two-way peg: deposits, withdrawals, tags, safety properties and trust model |
| [SIGNERS.md](SIGNERS.md) | Separate signer services and the `dogevm signer-setup` ceremony |
| [RUNBOOK.md](RUNBOOK.md) | What the operator does when something goes wrong: pause, alerts, refunds, restore |
| [FIRST-ROUND-TRIP.md](FIRST-ROUND-TRIP.md) | The record of the first mainnet round trip and the move to separate signers |
| [API.md](API.md) | The public HTTP API and JSON-RPC, for wallet and app developers ([metaldoge.com/developers](https://metaldoge.com/developers)) |
| [RUN-A-NODE.md](RUN-A-NODE.md) | Running a DogecoinVM node that follows the chain ([metaldoge.com/run-a-node](https://metaldoge.com/run-a-node)) |
| [CHANGELOG.md](CHANGELOG.md) | What changed in DogecoinVM, the bridge and the Mac wallet ([metaldoge.com/changelog](https://metaldoge.com/changelog)) |
| [dip-0021.md](dip-0021.md) | DIP 21 (draft): `dogecoin:` and `dogecoinvm:` payment links |

The user-facing docs are at [metaldoge.com/docs](https://metaldoge.com/docs), and the roadmap at [metaldoge.com/roadmap](https://metaldoge.com/roadmap).

## Makefile targets

- `make hooks` installs git hooks that refuse commits and pushes containing secrets.
- `make secretscan` scans every commit on every branch for secrets.
- `make build` and `make run_local` are inherited from upstream btcvm: they still use btcvm's VM ID and a five-node `metal-network-runner` setup. For a local DogecoinVM network, use `scripts/devnet.sh` as described in [TESTING.md](TESTING.md); it builds the plugin under DogecoinVM's VM ID.

## How the VM fits together

- `vm/vm.go` creates the VM: node context, database, mempool, block building, gossip and network handlers, and the methods metalgo calls (`Initialize`, `Shutdown`, `SetState`, `LastAccepted`, block parsing and verification).
- `vm/block_adapter.go` keeps btcd holding only accepted blocks; see "How it works" in the [main README](../README.md).
- The VM follows the pattern in Avalanche's guide to [complex Go VMs](https://build.avax.network/docs/virtual-machines/golang-vms/complex-golang-vm).
