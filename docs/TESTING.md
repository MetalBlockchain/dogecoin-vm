# Running and testing DogecoinVM

## Mainnet beta

**https://metaldoge.com** runs DogecoinVM as an L1 on Metal mainnet, pegged to Dogecoin mainnet with real DOGE:

| | |
|---|---|
| Metal subnet | `2t2zEB1T3mNUE2WoheMFMjfhAvQJawtgiwnKPJz2NsFk7FDgyN` |
| DogecoinVM chain | `2hFCfzdMmfXBxYgvvdL7BYiJAxdejyn4AksMYUM2eM5gN7Xrjy` |
| L1 conversion | `2Ns8AgaesW78Z8CwJvjzkdGpV7qnLTgEfBoXn9zDTcNNxx3phR` |
| Validator | `NodeID-5qeQJPktXm63RrXxxkfjunFJPQCP7YeeK` |
| Peg address (Dogecoin) | `AAvNfukpAa4iTcRJPetxuxX8XxbC5gFqUM` |
| JSON-RPC | `https://metaldoge.com/rpc`, user and password `public` |

It is a beta: deposits over 100 DOGE are not credited (they are held for a refund), at most 1,000 DOGE can circulate, deposits need 20 Dogecoin confirmations, and one server holds every peg signer key. It was set up with [`deploy/mainnet.sh`](../deploy/mainnet.sh) and [`cmd/dogevm-l1`](../cmd/dogevm-l1). Anyone can check the L1 on the P-Chain, for example with `platform.getL1Validator` and the validation ID `qPanCREE6TaXz1TFT2qbt9uKvjtHg1AzWNMzYnStYAeJshNmB`.

## Public testnet (retired)

The hosted test network used to run at **https://metaldoge.com**; the notes below describe it and still apply to a testnet you run yourself:

- **Web wallet.** Create a key (it stays in your browser), then use the faucet or deposit Dogecoin testnet DOGE and withdraw it back. The page shows the live peg audit: DOGE locked on Dogecoin against DOGE circulating on DogecoinVM.
- **JSON-RPC** at `https://metaldoge.com/rpc`, user `public`, password `public`. It can read and broadcast but not administer.
- **CLI.** Point `dogevm` at it:
  ```bash
  export DOGEVM_RPC=https://metaldoge.com/rpc DOGEVM_RPC_USER=public DOGEVM_RPC_PASS=public DOGEVM_NETWORK=testnet
  ./dogevm balance -address <your address>
  ```
- **Deposits.** Each DogecoinVM address gets its own Dogecoin testnet deposit address (Deposit tab). Send testnet DOGE there from any wallet, for example straight from a Dogecoin testnet faucet.

It runs one Metal node, Dogecoin Core on Dogecoin testnet, the bridge and the web wallet, set up by [`deploy/provision.sh`](../deploy/provision.sh). The rest of this page runs the same stack on your own machine.

## Local setup

This runs everything on your machine: a Metal node running DogecoinVM, Dogecoin Core on regtest, and a DOGE round trip through the peg. It was run on macOS (arm64); Linux works the same way.

### What you need

- Go 1.24+, `jq`, `openssl`, Docker (for Dogecoin Core).
- **metalgo v1.13.5.** The plugin speaks rpcchainvm protocol 43 and will not load in other versions.
  ```bash
  git clone --depth 1 --branch v1.13.5 https://github.com/MetalBlockchain/metalgo
  (cd metalgo && ./scripts/build.sh)          # -> metalgo/build/metalgo
  ```
- **Dogecoin Core 1.14.9.** On Apple silicon, run the official aarch64 Linux build in Docker:
  ```bash
  gh release download v1.14.9 -R dogecoin/dogecoin -p 'dogecoin-1.14.9-aarch64-linux-gnu.tar.gz'
  tar xzf dogecoin-1.14.9-aarch64-linux-gnu.tar.gz
  docker run -d --name dogecoind -p 127.0.0.1:18332:18332 \
    -v "$PWD/dogecoin-1.14.9:/opt/dogecoin:ro" -v "$PWD/doge-regtest:/data" \
    debian:bookworm-slim /opt/dogecoin/bin/dogecoind -regtest -datadir=/data \
    -rpcuser=doge -rpcpassword=doge -rpcallowip=0.0.0.0/0 -rpcbind=0.0.0.0 -txindex=1
  alias dcli='docker exec dogecoind /opt/dogecoin/bin/dogecoin-cli -regtest -datadir=/data -rpcuser=doge -rpcpassword=doge'
  dcli generate 110                             # mature some regtest coins
  ```
  Check the tarball against the release's `SHA256SUMS.asc`.

### 1. Start a DogecoinVM devnet

```bash
METALGO=/path/to/metalgo/build/metalgo scripts/devnet.sh start
eval "$(scripts/devnet.sh env)"
```

This builds the plugin under its VM ID (`mEUwHwfd8UTHf23UYkQxHvy1n1EGwWieXQnjmtzSryJRZckzu`) and creates a 2-of-3 peg signer set. It then starts a single metalgo node on a local network with sybil protection off, and creates a subnet and chain with a 1-block (9 billion DOGE) peg reserve. State lives in `~/.dogevm-devnet`; `scripts/devnet.sh stop` stops the node, and deleting the directory starts over.

### 2. Connect

DogecoinVM speaks btcd's JSON-RPC (Bitcoin Core-style) at:

```
http://127.0.0.1:9650/ext/bc/<chainID>/rpc      (HTTP basic auth: DOGEVM_RPC_USER / DOGEVM_RPC_PASS)
```

```bash
curl -s -u "$DOGEVM_RPC_USER:$DOGEVM_RPC_PASS" -H 'content-type: application/json' \
  -d '{"jsonrpc":"1.0","id":1,"method":"getblockcount","params":[]}' "$DOGEVM_RPC"
```

RPC credentials and indexes are node settings, in `~/.dogevm-devnet/chain-configs/<chainID>/config.json`, not in the public genesis. The node needs `txIndex` and `addrIndex` for the wallet and bridge. For a shared node, set `rpcLimitUser`/`rpcLimitPass` there: a limited user can read and broadcast but not administer.

### 3. Wallets

Keys and addresses are Dogecoin's, so one key controls the same funds' address on both chains. No existing Dogecoin wallet can talk to DogecoinVM yet, because it is not reachable over Dogecoin's P2P network or Electrum. Use the `dogevm` CLI for now:

```bash
go build -o dogevm ./cmd/dogevm
export DOGECOIN_RPC=http://127.0.0.1:18332 DOGECOIN_RPC_USER=doge DOGECOIN_RPC_PASS=doge

./dogevm keygen                                   # key, with DogecoinVM and Dogecoin addresses
./dogevm balance -address <dogecoinvm address>
./dogevm send -key <key> -to <dogecoinvm address> -amount 100
```

`keygen` prints the private key as a Dogecoin WIF too, so the same key can be imported into Dogecoin Core with `importprivkey`.

### 4. Peg in and out

```bash
SIGNERS=~/.dogevm-devnet/signers.json

# Run the bridge (in its own terminal); it polls both chains.
./dogevm bridge -signers $SIGNERS -rescan

# Peg in: deposit 1,000 DOGE from the Dogecoin Core wallet, credited to a DogecoinVM address.
./dogevm peg-in -signers $SIGNERS -to <dogecoinvm address> -amount 1000
dcli generate 6                                   # confirmations; the bridge then credits 999.99

# Peg out: send 500 DOGE back to a Dogecoin address.
./dogevm peg-out -signers $SIGNERS -key <key> -to <dogecoin address> -amount 500
dcli generate 1                                   # the bridge pays 499 (1 DOGE Dogecoin fee)

# Check the peg is fully backed.
./dogevm audit -signers $SIGNERS
```

Run on the devnet above, this gave:

```
credited 999.99000000 DOGE for deposit 6abed378…:1 in 91c73fb7…
paid 499.00000000 DOGE for peg-out 4fce1377… in 20c02315…
{ "circulating": "500.00000000", "locked": "500.00000000", "surplus": "0.00000000", "solvent": true, … }
```

See [BRIDGE.md](BRIDGE.md) for how the peg works and its trust model.

### Against Dogecoin testnet instead of regtest

Run `dogecoind -testnet -txindex=1` (RPC port 44555), wait for it to sync, and use `DOGECOIN_NETWORK=testnet` for `scripts/devnet.sh start` and `dogevm`. Testnet DOGE comes from a faucet. Set the bridge's `-confirmations` to the depth you want (at least 6).

## Automated tests

```bash
go test ./vm/ ./cmd/... ./btcd/ ./btcd/mempool/
```

- `vm/`: block lifecycle, peg reserve consensus and a multisig reserve release, run in-process against a real btcd.
- `cmd/dogevm/`: bridge logic against in-memory chains that run btcd's script engine on every signed input: exactly-once crediting, spoofed tags, personal deposit addresses, a round trip, and halting when insolvent. Also checks the web wallet's `chain.js` against the Go code under Node, when Node is installed.
