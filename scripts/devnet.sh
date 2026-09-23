#!/usr/bin/env bash
# Runs a single-node local Metal network with a DogecoinVM chain.
#
#   METALGO=/path/to/metalgo scripts/devnet.sh start   # build, start, create the chain
#   scripts/devnet.sh stop
#   scripts/devnet.sh env                              # print connection settings
#   METALGO=... scripts/devnet.sh run                  # run the node in the foreground (systemd)
#
# PUBLIC_RPC_USER and PUBLIC_RPC_PASS, if set when the chain is created, add a
# limited RPC user that can read and broadcast but not administer the node.
#
# The node runs with sybil protection disabled, so it alone validates every
# chain. State lives in DEVNET_DIR (default ~/.dogevm-devnet); delete it to
# start over. Requires metalgo v1.13.5 (rpcchainvm protocol 43).
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
DIR=${DEVNET_DIR:-$HOME/.dogevm-devnet}
DOGE_NETWORK=${DOGECOIN_NETWORK:-regtest}
HTTP_PORT=${HTTP_PORT:-9650}
URI="http://127.0.0.1:$HTTP_PORT"
RPC_USER=dogevm
RPC_PASS_FILE="$DIR/rpc-password"

log() { echo "devnet: $*" >&2; }

node_flags() {
  local flags=(
    --network-id=local
    --sybil-protection-enabled=false
    --snow-sample-size=1 --snow-quorum-size=1
    --data-dir="$DIR/node" --log-dir="$DIR/logs"
    --plugin-dir="$DIR/plugins" --chain-config-dir="$DIR/chain-configs"
    --chain-aliases-file="$DIR/aliases.json"
    --http-host=127.0.0.1 --http-port="$HTTP_PORT" --staking-port=$((HTTP_PORT + 1))
    --public-ip=127.0.0.1 --bootstrap-ips= --bootstrap-ids=
  )
  if [[ -f "$DIR/chain.json" ]]; then
    flags+=(--track-subnets="$(jq -r .subnetID "$DIR/chain.json")")
  fi
  printf '%s\n' "${flags[@]}"
}

start_node() {
  [[ -n "${METALGO:-}" && -x "$METALGO" ]] || { log "set METALGO to a metalgo v1.13.5 binary"; exit 1; }
  local flags=()
  while IFS= read -r f; do flags+=("$f"); done < <(node_flags)
  nohup "$METALGO" "${flags[@]}" >"$DIR/node.out" 2>&1 &
  echo $! >"$DIR/node.pid"
  log "node pid $(cat "$DIR/node.pid"), output in $DIR/node.out"
  for _ in $(seq 120); do
    if curl -sf -X POST -H 'content-type:application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"info.isBootstrapped","params":{"chain":"P"}}' \
      "$URI/ext/info" | grep -q '"isBootstrapped":true'; then
      return
    fi
    sleep 1
  done
  log "node did not bootstrap; see $DIR/node.out"; exit 1
}

stop_node() {
  if [[ -f "$DIR/node.pid" ]] && kill "$(cat "$DIR/node.pid")" 2>/dev/null; then
    while kill -0 "$(cat "$DIR/node.pid")" 2>/dev/null; do sleep 0.5; done
    log "node stopped"
  fi
  rm -f "$DIR/node.pid"
}

wait_chain() {
  for _ in $(seq 120); do
    if curl -sf -X POST -H 'content-type:application/json' \
      -d '{"jsonrpc":"2.0","id":1,"method":"info.isBootstrapped","params":{"chain":"dogecoinvm"}}' \
      "$URI/ext/info" | grep -q '"isBootstrapped":true'; then
      log "DogecoinVM chain is up"
      return
    fi
    sleep 1
  done
  log "chain did not start; see $DIR/logs"; exit 1
}

cmd_start() {
  mkdir -p "$DIR/plugins" "$DIR/bin" "$DIR/chain-configs"
  command -v jq >/dev/null || { log "jq is required"; exit 1; }

  log "building plugin and tools"
  VMID=$(cd "$ROOT" && go run ./scripts/vm-id-generator.go)
  (cd "$ROOT" && go build -o "$DIR/plugins/$VMID" ./cmd/dogevm-plugin \
    && go build -o "$DIR/bin/dogevm" ./cmd/dogevm \
    && go build -o "$DIR/bin/dogevm-devnet" ./cmd/dogevm-devnet)

  if [[ ! -f "$DIR/signers.json" ]]; then
    "$DIR/bin/dogevm" signers -required 2 -total 3 -out "$DIR/signers.json" \
      -doge-network "$DOGE_NETWORK" >"$DIR/signers.out"
    log "created peg signer set $DIR/signers.json"
  fi
  RESERVE=$(jq -r .dogecoinvmReserve "$DIR/signers.out")

  [[ -f "$RPC_PASS_FILE" ]] || openssl rand -hex 16 >"$RPC_PASS_FILE"
  [[ -f "$DIR/builder.json" ]] || "$DIR/bin/dogevm" keygen -doge-network "$DOGE_NETWORK" >"$DIR/builder.json"
  [[ -f "$DIR/aliases.json" ]] || echo '{}' >"$DIR/aliases.json"

  stop_node
  start_node

  if [[ ! -f "$DIR/chain.json" ]]; then
    jq -n --arg reserve "$RESERVE" \
      '{config: {testNet: true, pegReserveAddress: $reserve, pegReserveBlocks: 1}}' >"$DIR/genesis.json"
    log "creating subnet and chain"
    "$DIR/bin/dogevm-devnet" -uri "$URI" -genesis "$DIR/genesis.json" >"$DIR/chain.json"
    CHAIN_ID=$(jq -r .chainID "$DIR/chain.json")

    # Node-local chain config: private RPC credentials and the indexes the
    # wallet and bridge need.
    mkdir -p "$DIR/chain-configs/$CHAIN_ID"
    jq -n --arg user "$RPC_USER" --arg pass "$(cat "$RPC_PASS_FILE")" \
      --arg builder "$(jq -r .dogecoinvmAddress "$DIR/builder.json")" \
      --arg data "$DIR/chaindata" --arg logs "$DIR/chainlogs" \
        --arg luser "${PUBLIC_RPC_USER:-}" --arg lpass "${PUBLIC_RPC_PASS:-}" \
      '{rpcUser: $user, rpcPass: $pass, txIndex: true, addrIndex: true,
        miningAddrs: [$builder], dataDir: $data, logDir: $logs}
       + (if $luser != "" then {rpcLimitUser: $luser, rpcLimitPass: $lpass} else {} end)' \
      >"$DIR/chain-configs/$CHAIN_ID/config.json"
    jq -n --arg id "$CHAIN_ID" '{($id): ["dogecoinvm"]}' >"$DIR/aliases.json"

    # Tracking the new subnet and the alias need a restart.
    stop_node
    start_node
  fi
  wait_chain
  cmd_env
}

cmd_env() {
  cat <<EOF
export DOGEVM_RPC=$URI/ext/bc/$(jq -r .chainID "$DIR/chain.json")/rpc
export DOGEVM_RPC_USER=$RPC_USER
export DOGEVM_RPC_PASS=$(cat "$RPC_PASS_FILE")
export DOGEVM_NETWORK=testnet
export DOGECOIN_NETWORK=$DOGE_NETWORK
# signer set: $DIR/signers.json   chain: $(jq -c . "$DIR/chain.json" 2>/dev/null)
EOF
}

cmd_run() {
  [[ -n "${METALGO:-}" && -x "$METALGO" ]] || { log "set METALGO to a metalgo v1.13.5 binary"; exit 1; }
  local flags=()
  while IFS= read -r f; do flags+=("$f"); done < <(node_flags)
  exec "$METALGO" "${flags[@]}"
}

case "${1:-}" in
  start) cmd_start ;;
  stop) stop_node ;;
  env) cmd_env ;;
  run) cmd_run ;;
  *) echo "usage: $0 start|stop|env|run" >&2; exit 2 ;;
esac
