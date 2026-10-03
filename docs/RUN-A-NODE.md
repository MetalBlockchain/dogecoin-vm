# Run a DogecoinVM node

DogecoinVM is an L1 on Metal Blockchain. To read it without depending on
metaldoge.com (for an app, an explorer or a Dogebox pup), run a **Metal node
that tracks the DogecoinVM L1**. It follows the chain and serves its JSON-RPC
locally; it doesn't validate, hold any bridge key or need any stake.

| | |
|---|---|
| Metal network | mainnet (`--network-id=mainnet`) |
| metalgo | **v1.13.5** exactly (the plugin speaks rpcchainvm protocol 43 and won't load in other versions) |
| Subnet (L1) ID | `2t2zEB1T3mNUE2WoheMFMjfhAvQJawtgiwnKPJz2NsFk7FDgyN` |
| DogecoinVM chain ID | `2hFCfzdMmfXBxYgvvdL7BYiJAxdejyn4AksMYUM2eM5gN7Xrjy` |
| VM ID (plugin file name) | `mEUwHwfd8UTHf23UYkQxHvy1n1EGwWieXQnjmtzSryJRZckzu` |
| Plugin release | **v0.1.1**, the build mainnet runs (https://github.com/MetalBlockchain/dogecoin-vm, tag `v0.1.1`) |

## What you need

- Linux (these steps use Ubuntu/Debian and systemd) or macOS for testing.
- Go **1.24** (the module's toolchain is go1.24.8) and git, to build.
- Modest hardware: a small VPS is enough. With partial sync of the primary
  network, the Metal node's data is a few hundred MB today, and the DogecoinVM
  chain itself is tiny; leave room to grow (tens of GB of disk).
- Outbound internet. Inbound, only the staking port (default 9651/tcp) should
  be reachable, so the node can talk to peers.

## 1. Build metalgo v1.13.5

```sh
git clone --depth 1 --branch v1.13.5 https://github.com/MetalBlockchain/metalgo
(cd metalgo && ./scripts/build.sh)        # -> metalgo/build/metalgo
sudo install -m 755 metalgo/build/metalgo /usr/local/bin/metalgo
```

## 2. Build the DogecoinVM plugin

Build the release tag the network runs (v0.1.1); newer commits on the
default branch may not be deployed yet. The plugin file must be named after
the VM ID and sit in metalgo's plugin directory.

```sh
git clone --branch v0.1.1 https://github.com/MetalBlockchain/dogecoin-vm
cd dogecoin-vm
go run ./scripts/vm-id-generator.go       # prints mEUwHwfd8UTHf23UYkQxHvy1n1EGwWieXQnjmtzSryJRZckzu
sudo install -d /var/lib/metal/plugins
go build -o mEUwHwfd8UTHf23UYkQxHvy1n1EGwWieXQnjmtzSryJRZckzu ./cmd/dogevm-plugin
sudo install -m 755 mEUwHwfd8UTHf23UYkQxHvy1n1EGwWieXQnjmtzSryJRZckzu /var/lib/metal/plugins/
```

## 3. Chain config

Node-local settings for the DogecoinVM chain go in
`<chain-config-dir>/<chainID>/config.json`. They're private to your node; the
consensus settings come from the chain's genesis on the P-Chain, and you can't
override them.

```sh
sudo install -d /var/lib/metal/chain-configs/2hFCfzdMmfXBxYgvvdL7BYiJAxdejyn4AksMYUM2eM5gN7Xrjy
sudo tee /var/lib/metal/chain-configs/2hFCfzdMmfXBxYgvvdL7BYiJAxdejyn4AksMYUM2eM5gN7Xrjy/config.json >/dev/null <<'JSON'
{
  "rpcUser": "admin",
  "rpcPass": "CHANGE-ME-long-random",
  "rpcLimitUser": "public",
  "rpcLimitPass": "public",
  "txIndex": true,
  "addrIndex": true,
  "dataDir": "/var/lib/metal/dogecoinvm/data",
  "logDir": "/var/lib/metal/dogecoinvm/logs"
}
JSON
```

| Key | What it does |
|---|---|
| `rpcUser` / `rpcPass` | Full JSON-RPC access. Keep it private (or leave it out). Generate the password, e.g. `openssl rand -hex 24`. |
| `rpcLimitUser` / `rpcLimitPass` | A read-mostly login: blocks, transactions, `searchrawtransactions`, `sendrawtransaction` and similar; no wallet or admin methods. This is what metaldoge.com exposes as `public`/`public`. |
| `txIndex` | Index every transaction, so `getrawtransaction` works for any txid. |
| `addrIndex` | Index addresses, so `searchrawtransactions` can list an address's history. Wallets and explorers need it. |
| `dataDir`, `logDir` | Where the chain's database and logs go. Set them explicitly: otherwise they're derived from `$HOME`. |

**Refused keys.** These are consensus settings and the plugin refuses to start
if the chain config sets them: `mainNet`, `testNet`, `pegReserveAddress`,
`pegReserveBlocks`. Upcoming releases also refuse `regressionTest`, `simNet`,
`sigNet`, `sigNetChallenge`, `addCheckpoints` (consensus), and `dropAddrIndex`,
`dropTxIndex`, `dropCfIndex` (they would delete an index at every start), with
key names matched case-insensitively. `miningAddrs` only matters for a
validator, which builds blocks.

## 4. Run metalgo, tracking the L1

```sh
sudo useradd --system --home /var/lib/metal --shell /usr/sbin/nologin metal || true
sudo install -d -o metal -g metal /var/lib/metal /var/lib/metal/dogecoinvm
sudo chown -R metal:metal /var/lib/metal
```

`/etc/systemd/system/metal-dogecoinvm.service`:

```ini
[Unit]
Description=Metal node tracking the DogecoinVM L1
After=network-online.target
Wants=network-online.target

[Service]
User=metal
Group=metal
Environment=HOME=/var/lib/metal
ExecStart=/usr/local/bin/metalgo \
  --network-id=mainnet \
  --data-dir=/var/lib/metal/node \
  --log-dir=/var/lib/metal/node-logs \
  --plugin-dir=/var/lib/metal/plugins \
  --chain-config-dir=/var/lib/metal/chain-configs \
  --track-subnets=2t2zEB1T3mNUE2WoheMFMjfhAvQJawtgiwnKPJz2NsFk7FDgyN \
  --partial-sync-primary-network=true \
  --http-host=127.0.0.1 \
  --http-port=9650 \
  --staking-port=9651
# Stop metalgo first and let it shut each chain down in order; signalling the
# plugin directly could cut off its database before the last block is saved.
KillMode=mixed
Restart=on-failure
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now metal-dogecoinvm
journalctl -fu metal-dogecoinvm
```

Notes:
- `--partial-sync-primary-network=true` syncs only what an L1 node needs from
  Metal's primary network (the P-Chain), which keeps disk and bandwidth low.
- `--http-host=127.0.0.1` keeps the node's APIs local. To serve the JSON-RPC to
  others, put a reverse proxy in front that forwards only the chain's `/rpc`
  path, and expose the **limited** login, never `rpcUser`.
- The node finds the chain's genesis on the P-Chain; there's nothing else to
  download.

## 5. Check it's synced

```sh
# Node health, including the DogecoinVM chain's own health check
curl -s http://127.0.0.1:9650/ext/health | jq '.healthy'

# Is the chain bootstrapped?
curl -s -X POST -H 'content-type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"info.isBootstrapped","params":{"chain":"2hFCfzdMmfXBxYgvvdL7BYiJAxdejyn4AksMYUM2eM5gN7Xrjy"}}' \
  http://127.0.0.1:9650/ext/info

# DogecoinVM's height: compare with https://metaldoge.com/api/status (dogecoinvmHeight)
curl -s -u public:public -H 'content-type: application/json' \
  -d '{"jsonrpc":"1.0","id":1,"method":"getblockcount","params":[]}' \
  http://127.0.0.1:9650/ext/bc/2hFCfzdMmfXBxYgvvdL7BYiJAxdejyn4AksMYUM2eM5gN7Xrjy/rpc
```

When `getblockcount` matches the public figure, your node is caught up. Blocks
on DogecoinVM are final once accepted, so there's no reorg to wait out.

## 6. Point your app at it

The chain's JSON-RPC is at

```
http://127.0.0.1:9650/ext/bc/2hFCfzdMmfXBxYgvvdL7BYiJAxdejyn4AksMYUM2eM5gN7Xrjy/rpc
```

(btcd / Bitcoin Core style; WebSocket notifications at `…/ws`). What an app
needs:

| Need | RPC |
|---|---|
| Tip, blocks | `getblockcount`, `getblockhash`, `getblock` |
| A transaction | `getrawtransaction <txid> 1` (needs `txIndex`) |
| An address's history and UTXOs | `searchrawtransactions <address>` (needs `addrIndex`), then `gettxout` for what's unspent |
| Broadcast | `sendrawtransaction <hex>` |

The bridge (deposit addresses, withdrawals, proof of reserves) isn't part of a
node: use the bridge's API at metaldoge.com for that ([API.md](API.md)). Keys
and signing stay in your app, exactly as for Dogecoin: same addresses, same
transaction format, same signing.

## Updating

When a new DogecoinVM release is announced, rebuild the plugin from the tagged
commit (step 2), replace the file in the plugin directory, and restart the
service. metalgo itself stays at v1.13.5 until a release says otherwise.

## Help

Questions and issues: https://github.com/MetalBlockchain/dogecoin-vm. Report
vulnerabilities privately via
[GitHub security advisories](https://github.com/MetalBlockchain/dogecoin-vm/security/advisories/new).
