#!/usr/bin/env bash
# Runs DogecoinVM as an L1 on Metal mainnet, pegged to Dogecoin mainnet.
# Run as root on a host prepared by deploy/provision.sh:
#
#   deploy/mainnet.sh status    # what is synced and funded
#   deploy/mainnet.sh launch    # create the L1 and start everything
#
# Before launch:
#   - the Metal mainnet node (metal-mainnet.service) has synced the P-Chain;
#   - the P-Chain key in $SECRETS/p-chain-key.json holds enough METAL: about
#     5 METAL prepays the validator's continuous fee for several months;
#   - the peg signer set is in $SECRETS/signers.json.
# Dogecoin Core (dogecoind-main.service) may still be syncing: deposits are
# credited once it has caught up. Its data directory needs room for the whole
# chain with -txindex: about 260 GB in late 2026, and growing. On a small
# server, attach a volume (400 GB or more) and bind-mount it at
# /var/lib/dogecoin-main before the node starts syncing.
#
# Alerts go to the Slack or Discord webhook URL in $SECRETS/alert-webhook, and
# to Telegram if $SECRETS/telegram-token and $SECRETS/telegram-chat exist; https://<domain>/api/health serves the same checks for uptime
# monitors.
#
# Launch is safe to re-run; it reuses the chain it created. It caps what the
# bridge credits (MAX_DEPOSIT and MAX_CIRCULATING, in DOGE) because one
# process holds every peg signer key.
set -euo pipefail

STATE=/var/lib/metal-main
SECRETS=$STATE/secrets
BIN=/opt/dogevm/bin
DOGE_CONF=/var/lib/dogecoin-main/dogecoin.conf
DOMAIN=${DOMAIN:-metaldoge.com}
NODE_API=http://127.0.0.1:9660
MAX_DEPOSIT=${MAX_DEPOSIT:-100}
MAX_CIRCULATING=${MAX_CIRCULATING:-1000}
CONFIRMATIONS=${CONFIRMATIONS:-20}
VALIDATOR_BALANCE=${VALIDATOR_BALANCE:-5}

as_dogevm() { sudo -u dogevm env HOME=/opt/dogevm "$@"; }
koinu() { awk -v d="$1" 'BEGIN { printf "%.0f", d * 100000000 }'; }
info_call() {
  curl -s -X POST -H 'content-type: application/json' \
    -d "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"$1\",\"params\":${2:-{\}}}" "$NODE_API/ext/$3"
}

cmd_status() {
  echo "Metal mainnet node:  $(info_call info.getNodeID '{}' info | jq -r .result.nodeID)"
  echo "P-Chain synced:      $(info_call info.isBootstrapped '{"chain":"P"}' info | jq -r .result.isBootstrapped)"
  echo "P-Chain key:         $(jq -r .pChainAddress "$SECRETS/p-chain-key.json")"
  echo "P-Chain balance:     $(as_dogevm "$BIN/dogevm-l1" balance -key "$SECRETS/p-chain-key.json" -uri "$NODE_API" 2>&1)"
  local d="as_dogevm /opt/dogecoin/bin/dogecoin-cli -datadir=/var/lib/dogecoin-main"
  echo "Dogecoin mainnet:    block $($d getblockcount) of $($d getblockchaininfo | jq .headers)"
  echo "Peg address:         $(jq -r .dogecoinPegAddress "$SECRETS/signers.out")"
  [[ -f "$STATE/chain.json" ]] && echo "L1:                  $(jq -c . "$STATE/chain.json")"
  return 0
}

cmd_launch() {
  [[ "$(info_call info.isBootstrapped '{"chain":"P"}' info | jq -r .result.isBootstrapped)" == true ]] ||
    { echo "the Metal node has not synced the P-Chain yet"; exit 1; }

  local reserve builder chain subnet
  reserve=$(jq -r .dogecoinvmReserve "$SECRETS/signers.out")
  [[ -f "$SECRETS/builder.json" ]] || as_dogevm "$BIN/dogevm" keygen -vm-network mainnet -doge-network mainnet >"$SECRETS/builder.json"
  builder=$(jq -r .dogecoinvmAddress "$SECRETS/builder.json")

  if [[ ! -f "$STATE/chain.json" ]]; then
    jq -n --arg reserve "$reserve" \
      '{config: {mainNet: true, pegReserveAddress: $reserve, pegReserveBlocks: 1}}' >"$STATE/genesis.json"
    as_dogevm "$BIN/dogevm-l1" create -key "$SECRETS/p-chain-key.json" -genesis "$STATE/genesis.json" \
      -node-uri "$NODE_API" -validator-balance "$VALIDATOR_BALANCE" >"$STATE/chain.json.tmp"
    mv "$STATE/chain.json.tmp" "$STATE/chain.json"
  fi
  chain=$(jq -r .chainID "$STATE/chain.json")
  subnet=$(jq -r .subnetID "$STATE/chain.json")

  # Node-local chain config: private RPC credentials, indexes, block builder.
  [[ -f "$SECRETS/rpc-password" ]] || openssl rand -hex 24 >"$SECRETS/rpc-password"
  install -d -o dogevm -g dogevm "$STATE/chain-configs/$chain"
  jq -n --arg pass "$(cat "$SECRETS/rpc-password")" --arg builder "$builder" \
    --arg data "$STATE/chaindata" --arg logs "$STATE/chainlogs" \
    '{rpcUser: "dogevm", rpcPass: $pass, rpcLimitUser: "public", rpcLimitPass: "public",
      txIndex: true, addrIndex: true, miningAddrs: [$builder], dataDir: $data, logDir: $logs}' \
    >"$STATE/chain-configs/$chain/config.json"
  chown -R dogevm:dogevm "$STATE" && chmod 700 "$SECRETS"

  # Track the L1's subnet.
  if ! grep -q -- "--track-subnets=$subnet" /etc/systemd/system/metal-mainnet.service; then
    sed -i "s|--public-ip=|--track-subnets=$subnet --public-ip=|" /etc/systemd/system/metal-mainnet.service
  fi
  # Keep the plugin current.
  cp "/var/lib/dogevm/plugins/$(jq -r .vmID "$STATE/chain.json")" "$STATE/plugins/" 2>/dev/null || true

  cat >"$SECRETS/bridge.env" <<ENV
DOGEVM_RPC=$NODE_API/ext/bc/$chain/rpc
DOGEVM_RPC_USER=dogevm
DOGEVM_RPC_PASS=$(cat "$SECRETS/rpc-password")
DOGEVM_NETWORK=mainnet
DOGECOIN_RPC=http://127.0.0.1:22555
DOGECOIN_RPC_USER=dogevm
DOGECOIN_RPC_PASS=$(sed -n 's/^rpcpassword=//p' "$DOGE_CONF")
DOGECOIN_NETWORK=mainnet
ENV
  chown dogevm:dogevm "$SECRETS/bridge.env" && chmod 600 "$SECRETS/bridge.env"

  local policy="-signers $SECRETS/signers.json -confirmations $CONFIRMATIONS \
-max-deposit $(koinu "$MAX_DEPOSIT") -max-circulating $(koinu "$MAX_CIRCULATING") -doge-fee $(koinu 0.1) -confirmation-tiers 1:1,10:6,50:12"
  local health="-validation-id $(jq -r .validationID "$STATE/chain.json") -pchain-uri $NODE_API/ext/bc/P"
  [[ -f "$SECRETS/alert-webhook" ]] && health="$health -webhook $(cat "$SECRETS/alert-webhook")"
  local alerts=""
  [[ -f "$SECRETS/telegram-token" && -f "$SECRETS/telegram-chat" ]] &&
    alerts="-telegram-token-file $SECRETS/telegram-token -telegram-chat $(cat "$SECRETS/telegram-chat")"
  for unit in bridge web monitor; do
    local exec="$BIN/dogevm bridge $policy -interval 30s"
    [[ $unit == web ]] && exec="$BIN/dogevm serve $policy ${health% -webhook*} -doge-index $STATE/dogeindex -listen 127.0.0.1:8081"
    [[ $unit == monitor ]] && exec="$BIN/dogevm monitor $policy $health $alerts"
    cat >"/etc/systemd/system/dogevm-$unit-main.service" <<UNIT
[Unit]
Description=DogecoinVM $unit (Metal mainnet, Dogecoin mainnet)
After=metal-mainnet.service dogecoind-main.service

[Service]
User=dogevm
Environment=HOME=/opt/dogevm
EnvironmentFile=$SECRETS/bridge.env
ExecStart=$exec
Restart=always
RestartSec=15

[Install]
WantedBy=multi-user.target
UNIT
  done

  # metaldoge.com serves mainnet; the testnet stack is retired.
  cat >/etc/caddy/Caddyfile <<CADDY
$DOMAIN {
	header Strict-Transport-Security "max-age=31536000"
	# The macOS wallet's DMGs and signed update feed (dogecoin-vm-wallet's
	# publish-update.sh uploads them).
	handle_path /download/* {
		root * /var/www/metaldoge-downloads
		file_server
	}
	handle /rpc {
		rewrite * /ext/bc/$chain/rpc
		reverse_proxy 127.0.0.1:9660 {
			header_up Host localhost
		}
	}
	handle {
		reverse_proxy 127.0.0.1:8081
	}
}
CADDY
  systemctl disable --now dogevm-node dogevm-bridge dogevm-web >/dev/null 2>&1 || true
  systemctl daemon-reload
  systemctl restart metal-mainnet
  systemctl enable --now dogevm-bridge-main dogevm-web-main dogevm-monitor-main >/dev/null
  systemctl restart dogevm-bridge-main dogevm-web-main dogevm-monitor-main caddy
  cmd_status
}

case "${1:-}" in
  status) cmd_status ;;
  launch) cmd_launch ;;
  *) echo "usage: $0 status|launch" >&2; exit 2 ;;
esac
