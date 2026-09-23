#!/usr/bin/env bash
# Provisions a public DogecoinVM test network on a fresh Ubuntu 24.04 x86_64
# host. Run as root:
#
#   DOMAIN=example.org deploy/provision.sh
#
# It installs Go, metalgo v1.13.5, DogecoinVM and Dogecoin Core 1.14.9 (on
# Dogecoin testnet), creates the DogecoinVM chain on a single-node network,
# and runs the node, Dogecoin Core, the peg bridge and the web wallet under
# systemd. Caddy serves the web wallet at https://<ip>.sslip.io and, if
# DOMAIN is set, at https://$DOMAIN, each with the chain's JSON-RPC at /rpc
# for a limited user that can read and broadcast but not administer.
#
# This is a test network: one process holds every peg signer key.
set -euo pipefail

REPO=${REPO:-https://github.com/MetalBlockchain/dogecoin-vm}
BRANCH=${BRANCH:-dogecoin}
GO_VERSION=1.24.11
GO_SHA256=bceca00afaac856bc48b4cc33db7cd9eb383c81811379faed3bdbc80edb0af65
METALGO_VERSION=v1.13.5
DOGECOIN_VERSION=1.14.9
DOGECOIN_SHA256=4f227117b411a7c98622c970986e27bcfc3f547a72bef65e7d9e82989175d4f8
PUBLIC_RPC_USER=${PUBLIC_RPC_USER:-public}
PUBLIC_RPC_PASS=${PUBLIC_RPC_PASS:-public}

IP=$(curl -s4 https://ifconfig.me)
SSLIP=${IP//./-}.sslip.io
DOMAIN=${DOMAIN:-}
HOME_DIR=/opt/dogevm
STATE=/var/lib/dogevm
DOGE_DATA=/var/lib/dogecoin

log() { echo "provision: $*"; }

log "packages"
export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -yq git jq curl build-essential caddy ufw openssl

if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go$GO_VERSION "; then
  log "Go $GO_VERSION"
  curl -sSLo /tmp/go.tgz "https://go.dev/dl/go$GO_VERSION.linux-amd64.tar.gz"
  echo "$GO_SHA256  /tmp/go.tgz" | sha256sum -c -
  rm -rf /usr/local/go && tar -C /usr/local -xzf /tmp/go.tgz
fi
export PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local

id dogevm >/dev/null 2>&1 || useradd --system --create-home --home-dir "$HOME_DIR" --shell /usr/sbin/nologin dogevm
install -d -o dogevm -g dogevm "$STATE" "$DOGE_DATA"
as_dogevm() { sudo -u dogevm env PATH="$PATH" GOTOOLCHAIN=local HOME="$HOME_DIR" "$@"; }

log "metalgo $METALGO_VERSION"
if [[ ! -x "$HOME_DIR/metalgo/build/metalgo" ]]; then
  as_dogevm git clone -q --depth 1 --branch "$METALGO_VERSION" https://github.com/MetalBlockchain/metalgo "$HOME_DIR/metalgo"
  (cd "$HOME_DIR/metalgo" && as_dogevm ./scripts/build.sh)
fi

log "DogecoinVM from $REPO@$BRANCH"
if [[ -d "$HOME_DIR/src/.git" ]]; then
  (cd "$HOME_DIR/src" && as_dogevm git fetch -q origin "$BRANCH" && as_dogevm git reset -q --hard "origin/$BRANCH")
else
  as_dogevm git clone -q --branch "$BRANCH" "$REPO" "$HOME_DIR/src"
fi
(cd "$HOME_DIR/src" && as_dogevm go build -o "$HOME_DIR/bin/dogevm" ./cmd/dogevm)
ln -sf "$HOME_DIR/bin/dogevm" /usr/local/bin/dogevm

log "Dogecoin Core $DOGECOIN_VERSION (testnet)"
if [[ ! -x /opt/dogecoin/bin/dogecoind ]]; then
  curl -sSLo /tmp/dogecoin.tgz "https://github.com/dogecoin/dogecoin/releases/download/v$DOGECOIN_VERSION/dogecoin-$DOGECOIN_VERSION-x86_64-linux-gnu.tar.gz"
  echo "$DOGECOIN_SHA256  /tmp/dogecoin.tgz" | sha256sum -c -
  tar -C /tmp -xzf /tmp/dogecoin.tgz
  rm -rf /opt/dogecoin && mv "/tmp/dogecoin-$DOGECOIN_VERSION" /opt/dogecoin
fi
if [[ ! -f "$DOGE_DATA/dogecoin.conf" ]]; then
  cat >"$DOGE_DATA/dogecoin.conf" <<EOF
testnet=1
server=1
txindex=1
rpcbind=127.0.0.1
rpcallowip=127.0.0.1
rpcuser=dogevm
rpcpassword=$(openssl rand -hex 24)
EOF
  chown dogevm:dogevm "$DOGE_DATA/dogecoin.conf" && chmod 600 "$DOGE_DATA/dogecoin.conf"
fi
DOGE_RPC_PASS=$(sed -n 's/^rpcpassword=//p' "$DOGE_DATA/dogecoin.conf")

cat >/etc/systemd/system/dogecoind.service <<EOF
[Unit]
Description=Dogecoin Core (testnet)
After=network-online.target

[Service]
User=dogevm
ExecStart=/opt/dogecoin/bin/dogecoind -datadir=$DOGE_DATA -printtoconsole
Restart=on-failure
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
EOF

log "DogecoinVM chain"
METALGO="$HOME_DIR/metalgo/build/metalgo"
if [[ ! -f "$STATE/chain.json" ]]; then
  (cd "$HOME_DIR/src" && as_dogevm env METALGO="$METALGO" DEVNET_DIR="$STATE" \
    DOGECOIN_NETWORK=testnet PUBLIC_RPC_USER="$PUBLIC_RPC_USER" PUBLIC_RPC_PASS="$PUBLIC_RPC_PASS" \
    scripts/devnet.sh start)
  (cd "$HOME_DIR/src" && as_dogevm env DEVNET_DIR="$STATE" scripts/devnet.sh stop)
else
  # Rebuild the plugin so an update takes effect on restart.
  VMID=$(cd "$HOME_DIR/src" && as_dogevm go run ./scripts/vm-id-generator.go)
  (cd "$HOME_DIR/src" && as_dogevm go build -o "$STATE/plugins/$VMID" ./cmd/dogevm-plugin)
fi
CHAIN_ID=$(jq -r .chainID "$STATE/chain.json")
VM_RPC_PASS=$(cat "$STATE/rpc-password")

cat >/etc/systemd/system/dogevm-node.service <<EOF
[Unit]
Description=Metal node running DogecoinVM
After=network-online.target

[Service]
User=dogevm
# The plugin and btcd derive default paths from HOME.
Environment=HOME=$HOME_DIR METALGO=$METALGO DEVNET_DIR=$STATE
WorkingDirectory=$HOME_DIR
ExecStart=$HOME_DIR/src/scripts/devnet.sh run
Restart=on-failure
TimeoutStopSec=120

[Install]
WantedBy=multi-user.target
EOF

cat >"$STATE/bridge.env" <<EOF
DOGEVM_RPC=http://127.0.0.1:9650/ext/bc/$CHAIN_ID/rpc
DOGEVM_RPC_USER=dogevm
DOGEVM_RPC_PASS=$VM_RPC_PASS
DOGEVM_NETWORK=testnet
DOGECOIN_RPC=http://127.0.0.1:44555
DOGECOIN_RPC_USER=dogevm
DOGECOIN_RPC_PASS=$DOGE_RPC_PASS
DOGECOIN_NETWORK=testnet
EOF
chown dogevm:dogevm "$STATE/bridge.env" && chmod 600 "$STATE/bridge.env"

cat >/etc/systemd/system/dogevm-bridge.service <<EOF
[Unit]
Description=DogecoinVM peg bridge (testnet)
After=dogevm-node.service dogecoind.service
Requires=dogevm-node.service dogecoind.service

[Service]
User=dogevm
Environment=HOME=$HOME_DIR
EnvironmentFile=$STATE/bridge.env
ExecStart=/usr/local/bin/dogevm bridge -signers $STATE/signers.json -confirmations 6 -interval 30s
Restart=always
RestartSec=30

[Install]
WantedBy=multi-user.target
EOF

# The faucet's DogecoinVM key. Fund it by pegging in testnet DOGE.
[[ -f "$STATE/faucet.json" ]] || as_dogevm dogevm keygen -doge-network testnet >"$STATE/faucet.json"
chown dogevm:dogevm "$STATE/faucet.json" && chmod 600 "$STATE/faucet.json"
FAUCET_KEY=$(jq -r .privateKeyHex "$STATE/faucet.json")

cat >/etc/systemd/system/dogevm-web.service <<UNIT
[Unit]
Description=DogecoinVM web wallet and API (testnet)
After=dogevm-node.service dogecoind.service

[Service]
User=dogevm
Environment=HOME=$HOME_DIR
EnvironmentFile=$STATE/bridge.env
ExecStart=/usr/local/bin/dogevm serve -signers $STATE/signers.json -listen 127.0.0.1:8080 -confirmations 6 -faucet-key $FAUCET_KEY -faucet-amount 100
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
UNIT

log "Caddy"
# Only the web wallet and the chain's JSON-RPC are public; metalgo's own
# APIs are not.
site() {
  cat <<SITE
$1 {
	header Strict-Transport-Security "max-age=31536000"
	handle /rpc {
		rewrite * /ext/bc/$CHAIN_ID/rpc
		reverse_proxy 127.0.0.1:9650 {
			header_up Host localhost
		}
	}
	handle {
		reverse_proxy 127.0.0.1:8080
	}
}
SITE
}
{
  site "$SSLIP"
  [[ -n "$DOMAIN" ]] && site "$DOMAIN"
  true
} >/etc/caddy/Caddyfile

ufw allow OpenSSH >/dev/null
ufw allow 80,443/tcp >/dev/null
ufw --force enable >/dev/null

systemctl daemon-reload
systemctl enable --now dogecoind dogevm-node >/dev/null
systemctl restart dogevm-node
systemctl enable dogevm-bridge dogevm-web >/dev/null
systemctl restart dogevm-web
systemctl restart caddy

log "done"
cat <<EOF

Web wallet:           https://$SSLIP${DOMAIN:+  and  https://$DOMAIN}
DogecoinVM JSON-RPC:  https://$SSLIP/rpc${DOMAIN:+  and  https://$DOMAIN/rpc}   (user $PUBLIC_RPC_USER, password $PUBLIC_RPC_PASS)
Faucet address:       $(jq -r .dogecoinvmAddress "$STATE/faucet.json")   (fund it by pegging in)
Chain ID:             $CHAIN_ID
Peg address:          $(jq -r .dogecoinPegAddress "$STATE/signers.out")   (Dogecoin testnet)
Reserve address:      $(jq -r .dogecoinvmReserve "$STATE/signers.out")   (DogecoinVM)

Dogecoin Core is syncing testnet. Start the bridge once it has caught up:
  systemctl start dogevm-bridge
EOF
