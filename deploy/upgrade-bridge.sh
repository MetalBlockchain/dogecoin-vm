#!/usr/bin/env bash
# Upgrades a running DogecoinVM bridge's software in place: the coordinator
# (bridge, web wallet, monitor) and its separate signers, together, since
# the two must speak the same request format. It touches nothing else: the
# Metal node, Dogecoin Core, keys, registries and logs stay as they are. Run
# as root on the bridge's host:
#
#   deploy/upgrade-bridge.sh            # build origin/$BRANCH
#   deploy/upgrade-bridge.sh COMMIT     # build that commit
#
# Steps, stopping at the first failure:
#   1. build the new dogevm binary beside the old one, as the dogevm user;
#   2. pause the bridge (a public reason shows on the site), and stop the
#      bridge, web wallet, monitor and every signer;
#   3. install the new binary for the coordinator and the signers;
#   4. check each signer's signing log, and rebuild one that is missing from
#      the chains (signer-log adopt): a signer refuses to start without one;
#   5. start the signers and check each answers, then the bridge, web
#      wallet and monitor;
#   6. resume, and show the health checks.
#
# If anything fails after step 2, the old binary goes back, everything is
# started again, and the bridge stays paused for the operator to look at:
# resume it with  dogevm resume -signers $SET  once it's understood.
set -euo pipefail

COMMIT=${1:-}
BRANCH=${BRANCH:-dogecoin}
HOME_DIR=/opt/dogevm
STATE=/var/lib/metal-main
SET=${SET:-$STATE/secrets/separate-signers/signers.json}
SIGNER_BIN=/usr/local/lib/dogevm/dogevm
REASON=${REASON:-"Upgrading the bridge software. Funds are safe; deposits and withdrawals resume in a few minutes."}

log() { printf '\n== %s\n' "$*"; }
die() { echo "upgrade-bridge: $*" >&2; exit 1; }
as_dogevm() { runuser -u dogevm -- env HOME=$HOME_DIR PATH="$HOME_DIR/go/bin:$PATH" "$@"; }

[[ $(id -u) == 0 ]] || die "run as root"
[[ -f $SET ]] || die "no signer set at $SET"
mapfile -t SIGNERS < <(systemctl list-unit-files --no-legend 'dogevm-signer-*.service' | awk '{print $1}' | sed 's/\.service$//' | sort -V)
COORD=(dogevm-bridge-main dogevm-web-main dogevm-monitor-main)

log "building dogevm"
(
  cd "$HOME_DIR/src"
  as_dogevm git fetch -q origin "$BRANCH"
  if [[ -n $COMMIT ]]; then as_dogevm git checkout -q --detach "$COMMIT"; else as_dogevm git checkout -q --detach "origin/$BRANCH"; fi
  as_dogevm git log -1 --format='%h %s'
  as_dogevm go build -o "$HOME_DIR/bin/dogevm.new" ./cmd/dogevm
)
NEW=$HOME_DIR/bin/dogevm.new
"$NEW" -h >/dev/null 2>&1 || true

log "pausing and stopping"
as_dogevm "$HOME_DIR/bin/dogevm" pause -signers "$SET" -reason "$REASON" || true
systemctl stop "${COORD[@]}"
systemctl stop "${SIGNERS[@]}"

cp -p "$HOME_DIR/bin/dogevm" "$HOME_DIR/bin/dogevm.prev"
cp -p "$SIGNER_BIN" "$SIGNER_BIN.prev"
rollback() {
  echo "upgrade-bridge: failed; putting the previous binary back and leaving the bridge paused" >&2
  cp -p "$HOME_DIR/bin/dogevm.prev" "$HOME_DIR/bin/dogevm"
  cp -p "$SIGNER_BIN.prev" "$SIGNER_BIN"
  systemctl start "${SIGNERS[@]}" "${COORD[@]}" || true
  as_dogevm "$HOME_DIR/bin/dogevm" pause -signers "$SET" -reason "$REASON" || true
}
trap rollback ERR

log "installing"
install -o dogevm -g dogevm -m 755 "$NEW" "$HOME_DIR/bin/dogevm"
install -o root -g root -m 755 "$NEW" "$SIGNER_BIN"
rm -f "$NEW"

log "signing logs"
for unit in "${SIGNERS[@]}"; do
  dir=/var/lib/$unit
  start=$(systemctl show -p ExecStart --value "$unit")
  arg() { sed -nE "s/.* $1 ([^ ;]+).*/\1/p" <<<"$start"; }
  signers=$(arg -signers) key=$(arg -key-file) logfile=$(arg -log) deposits=$(arg -deposits)
  [[ -n $signers && -n $key && -n $logfile ]] || die "$unit: can't read its -signers, -key-file and -log"
  step=check
  [[ -f $logfile ]] || step=adopt
  echo "$unit: signer-log $step"
  runuser -u "$unit" -- bash -c 'set -a; . "$1"; set +a; shift; exec "$@"' _ "$dir/signer.env" \
    "$SIGNER_BIN" signer-log "$step" -signers "$signers" -key-file "$key" -log "$logfile" ${deposits:+-deposits "$deposits"}
done

log "cosigners.json"
# The coordinator now names the signer each request is for, so it needs
# every signer's public key. Sets assembled before that list only URLs:
# fill each key in from the signer's own card, matched by its listen port.
COSIGNERS=$(dirname "$SET")/cosigners.json
if [[ -f $COSIGNERS ]]; then
  pairs=()
  for unit in "${SIGNERS[@]}"; do
    start=$(systemctl show -p ExecStart --value "$unit")
    listen=$(sed -nE 's/.* -listen ([^ ;]+).*/\1/p' <<<"$start")
    card=/var/lib/$unit/card.json
    [[ -n $listen && -f $card ]] && pairs+=("$listen" "$card")
  done
  python3 - "$COSIGNERS" "${pairs[@]}" <<'PY'
import json, sys
path, rest = sys.argv[1], sys.argv[2:]
cards = {rest[i]: json.load(open(rest[i + 1]))["publicKey"] for i in range(0, len(rest), 2)}
signers = json.load(open(path))
changed = False
for s in signers:
    host = s["url"].split("://", 1)[-1].rstrip("/")
    if not s.get("publicKey") and host in cards:
        s["publicKey"] = cards[host]
        changed = True
    print(s["url"], "->", s.get("publicKey", "(no key)")[:16] + "...")
if changed:
    json.dump(signers, open(path + ".new", "w"), indent=2)
PY
  if [[ -f $COSIGNERS.new ]]; then
    cp -p "$COSIGNERS" "$COSIGNERS.prev"
    chown --reference="$COSIGNERS" "$COSIGNERS.new" && chmod --reference="$COSIGNERS" "$COSIGNERS.new"
    mv "$COSIGNERS.new" "$COSIGNERS"
  fi
fi

log "starting signers"
systemctl start "${SIGNERS[@]}"
sleep 3
for unit in "${SIGNERS[@]}"; do
  systemctl is-active --quiet "$unit" || die "$unit didn't start: journalctl -u $unit"
done

log "starting the bridge"
systemctl start "${COORD[@]}"
sleep 5
for unit in "${COORD[@]}"; do
  systemctl is-active --quiet "$unit" || die "$unit didn't start: journalctl -u $unit"
done
trap - ERR

log "resuming"
as_dogevm "$HOME_DIR/bin/dogevm" resume -signers "$SET"
sleep 10
curl -s http://127.0.0.1:8081/api/health || true
echo
echo "upgrade-bridge: done. Old binaries are kept as $HOME_DIR/bin/dogevm.prev and $SIGNER_BIN.prev."
