#!/usr/bin/env bash
# Encrypted backups of everything a DogecoinVM mainnet host can't rebuild from
# the chains: the peg signer set, the P-Chain key, the validator's staking
# identity, the deposit address registry, the watch-only Dogecoin wallet, and
# the service configuration. Run as root on the host:
#
#   deploy/backup.sh setup AGE_RECIPIENT...   # install a daily backup
#   deploy/backup.sh run                      # back up now
#   deploy/backup.sh list
#
# Backups are encrypted with age to the recipients' public keys. The host
# holds no decryption key, so it can write backups but not read them. Keep
# the age identity (private key) offline, e.g. in a password manager.
#
# Each backup lands in $BACKUP_DIR; the newest $KEEP are kept. If
# /etc/dogevm-backup/offsite holds an rsync destination (for example a
# Hetzner Storage Box, user@host:dir), each backup is copied there too.
# scripts/pull-backups.sh copies them to another machine, and
# scripts/restore-check.sh proves one restores.
set -euo pipefail

STATE=/var/lib/metal-main
SECRETS=$STATE/secrets
DOGE_DIR=/var/lib/dogecoin-main
CONF=/etc/dogevm-backup
BACKUP_DIR=${BACKUP_DIR:-/var/backups/dogevm}
KEEP=${KEEP:-30}

log() { echo "backup: $*"; }

cmd_setup() {
  [[ $# -ge 1 ]] || { echo "usage: $0 setup AGE_RECIPIENT..." >&2; exit 2; }
  command -v age >/dev/null || DEBIAN_FRONTEND=noninteractive apt-get install -yq age >/dev/null
  install -d -m 700 "$CONF" "$BACKUP_DIR"
  printf '%s\n' "$@" >"$CONF/recipients"
  install -m 755 "$(readlink -f "$0")" /usr/local/sbin/dogevm-backup
  cat >/etc/systemd/system/dogevm-backup.service <<'UNIT'
[Unit]
Description=Encrypted backup of DogecoinVM keys and configuration

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/dogevm-backup run
UNIT
  cat >/etc/systemd/system/dogevm-backup.timer <<'UNIT'
[Unit]
Description=Daily DogecoinVM backup

[Timer]
OnCalendar=*-*-* 03:17:00 UTC
RandomizedDelaySec=20m
Persistent=true

[Install]
WantedBy=timers.target
UNIT
  systemctl daemon-reload
  systemctl enable --now dogevm-backup.timer >/dev/null
  log "daily backups to $BACKUP_DIR, encrypted to $# recipient(s)"
  cmd_run
}

cmd_run() {
  [[ -s "$CONF/recipients" ]] || { echo "no recipients; run: $0 setup AGE_RECIPIENT" >&2; exit 1; }
  local stamp out
  stamp=$(date -u +%Y%m%dT%H%M%SZ)
  # Global, so the exit trap can still see it.
  WORK=$(mktemp -d)
  trap 'rm -rf "$WORK"' EXIT
  local work=$WORK
  mkdir -p "$work/dogevm"
  local root=$work/dogevm

  # Files, copied with their paths.
  local files=(
    "$SECRETS"
    "$STATE/chain.json" "$STATE/genesis.json" "$STATE/chain-configs"
    "$STATE/node/staking"
    "$DOGE_DIR/dogecoin.conf"
    /etc/caddy/Caddyfile
  )
  for unit in /etc/systemd/system/{metal-mainnet,dogecoind-main,dogevm-bridge-main,dogevm-web-main,dogevm-monitor-main,dogevm-signer-1,dogevm-signer-2,dogevm-signer-3}.service; do
    [[ -f $unit ]] && files+=("$unit")
  done
  for f in "${files[@]}"; do
    [[ -e $f ]] || { log "missing $f"; continue; }
    mkdir -p "$root$(dirname "$f")"
    cp -a "$f" "$root$(dirname "$f")/"
  done

  # The watch-only wallet, copied consistently by Dogecoin Core itself.
  # Rebuilding it instead means a rescan of the whole chain.
  # Dogecoin Core 1.14 writes wallet backups into its own backups/ folder,
  # whatever path it is given.
  local wallet=dogevm-wallet-$stamp.dat
  if sudo -u dogevm /opt/dogecoin/bin/dogecoin-cli -datadir="$DOGE_DIR" backupwallet "$wallet" 2>/dev/null &&
    [[ -f "$DOGE_DIR/backups/$wallet" ]]; then
    mkdir -p "$root$DOGE_DIR"
    mv "$DOGE_DIR/backups/$wallet" "$root$DOGE_DIR/wallet.dat"
  else
    log "could not back up the Dogecoin wallet (is dogecoind running?)"
  fi

  # A manifest, so a restore can be checked against what was live.
  local node_id peg
  node_id=$(curl -s -m 10 -X POST -H 'content-type: application/json' \
    -d '{"jsonrpc":"2.0","id":1,"method":"info.getNodeID"}' http://127.0.0.1:9660/ext/info | jq -r '.result.nodeID // empty')
  peg=$(jq -r '.dogecoinPegAddress // empty' "$SECRETS/signers.out" 2>/dev/null || true)
  (cd "$root" && find . -type f ! -name MANIFEST.json -print0 | sort -z | xargs -0 sha256sum) >"$work/sums"
  jq -n --arg host "$(hostname)" --arg time "$stamp" --arg nodeID "$node_id" --arg peg "$peg" \
    --rawfile sums "$work/sums" \
    '{host: $host, time: $time, nodeID: $nodeID, dogecoinPegAddress: $peg, sha256: $sums}' >"$root/MANIFEST.json"

  out=$BACKUP_DIR/dogevm-$stamp.tar.age
  install -d -m 700 "$BACKUP_DIR"
  local recipients=()
  while read -r r; do [[ -n $r && $r != \#* ]] && recipients+=(-r "$r"); done <"$CONF/recipients"
  tar -C "$work" -czf - dogevm | age "${recipients[@]}" -o "$out.partial"
  chmod 600 "$out.partial"
  mv "$out.partial" "$out"
  log "wrote $out ($(du -h "$out" | cut -f1))"

  # Keep the newest $KEEP.
  ls -1t "$BACKUP_DIR"/dogevm-*.tar.age | tail -n +$((KEEP + 1)) | xargs -r rm -f

  if [[ -s "$CONF/offsite" ]]; then
    rsync -a --chmod=F600 "$out" "$(cat "$CONF/offsite")/" && log "copied offsite"
  fi
}

cmd_list() {
  ls -lh "$BACKUP_DIR"/dogevm-*.tar.age 2>/dev/null || echo "no backups yet"
  systemctl list-timers dogevm-backup.timer --no-pager 2>/dev/null | head -2
}

case "${1:-}" in
  setup) shift; cmd_setup "$@" ;;
  run) cmd_run ;;
  list) cmd_list ;;
  *) echo "usage: $0 setup AGE_RECIPIENT... | run | list" >&2; exit 2 ;;
esac
