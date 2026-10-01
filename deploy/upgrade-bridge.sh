#!/usr/bin/env bash
# Upgrades a running DogecoinVM bridge's software in place: the coordinator
# (bridge, web wallet, monitor) and its separate signers, together, since
# the two must speak the same request format. It touches nothing else: the
# Metal node, Dogecoin Core, keys and registries stay as they are. Run as
# root on the bridge's host:
#
#   deploy/upgrade-bridge.sh COMMIT     # build that commit (from origin/$BRANCH)
#
# Steps, stopping at the first failure:
#   1. build the new dogevm binary beside the old one, as the dogevm user;
#   2. pause the bridge (a public reason shows on the site); this must
#      succeed, and a pause already in place is kept;
#   3. back up every signer's signing log, cosigners.json and the binaries,
#      then stop the bridge, web wallet, monitor and every signer;
#   4. install the new binary for the coordinator and the signers;
#   5. check every signer's signing log (it must exist, match the chains,
#      and not be quarantined). A missing log stops the upgrade: a signer
#      that has signed must have its log restored from a backup. Only a
#      signer named in INIT_LOGS (its key never signed) gets a new empty
#      log, and only one in ADOPT_LOGS (it signed only before separate
#      signers existed, so all its signatures are on the chains) gets one
#      rebuilt from the chains;
#   6. fill each signer's public key into cosigners.json from its own card
#      where missing, and check every key there is in the signer set;
#   7. start the signers, then the bridge, web wallet and monitor, and check
#      the peg's audit is solvent.
#
# It never resumes the bridge unless RESUME=1 and the bridge wasn't already
# paused before the upgrade. Otherwise read the checks, then:
#   runuser -u dogevm -- /opt/dogevm/bin/dogevm resume -signers $SET
#
# On any failure after step 2, the previous binaries, logs and
# cosigners.json go back, everything is started again, and the bridge stays
# paused.
set -euo pipefail

CHAIN=dogevm
COMMIT=${1:?usage: deploy/upgrade-bridge.sh COMMIT}
BRANCH=${BRANCH:-dogecoin}
HOME_DIR=${HOME_DIR:-/opt/$CHAIN}
STATE=${STATE:-/var/lib/metal-main}
VARLIB=${VARLIB:-/var/lib}
BACKUPS=${BACKUPS:-/root}
SET=${SET:-$STATE/secrets/separate-signers/signers.json}
SIGNER_BIN=${SIGNER_BIN:-/usr/local/lib/$CHAIN/$CHAIN}
INIT_LOGS=${INIT_LOGS:-}
ADOPT_LOGS=${ADOPT_LOGS:-}
RESUME=${RESUME:-0}
REASON=${REASON:-"Upgrading the bridge software. Funds are safe; deposits and withdrawals resume shortly."}

log() { printf '\n== %s\n' "$*"; }
die() { echo "upgrade-bridge: $*" >&2; exit 1; }
as_user() { runuser -u "$CHAIN" -- env HOME="$HOME_DIR" PATH="/usr/local/go/bin:$PATH" GOTOOLCHAIN=local "$@"; }
listed() { [[ " $1 " == *" $2 "* ]]; }
paused_now() { [[ -f $(dirname "$SET")/paused.json ]]; }
listen_of() { sed -nE 's/.* -listen ([^ ;]+).*/\1/p' <<<"$(systemctl show -p ExecStart --value "$1")"; }

[[ $(id -u) == 0 || -n ${UPGRADE_TEST:-} ]] || die "run as root"
[[ -f $SET ]] || die "no signer set at $SET"
COSIGNERS=$(dirname "$SET")/cosigners.json
mapfile -t SIGNERS < <(systemctl list-unit-files --no-legend "$CHAIN-signer-*.service" | awk '{print $1}' | sed 's/\.service$//' | sort -V)
COORD=("$CHAIN-bridge-main" "$CHAIN-web-main" "$CHAIN-monitor-main")
WEB=$(listen_of "$CHAIN-web-main")

log "building $CHAIN $COMMIT"
(
  cd "$HOME_DIR/src"
  as_user git fetch -q origin "$BRANCH"
  as_user git checkout -q --detach "$COMMIT"
  as_user git log -1 --format='%H %s'
  as_user go build -o "$HOME_DIR/bin/$CHAIN.new" "./cmd/$CHAIN"
)
NEW=$HOME_DIR/bin/$CHAIN.new

log "pausing"
WAS_PAUSED=0
if paused_now; then WAS_PAUSED=1; echo "already paused: it stays paused afterwards"; fi
if [[ $WAS_PAUSED == 0 ]]; then
  as_user "$HOME_DIR/bin/$CHAIN" pause -signers "$SET" -reason "$REASON"
fi
paused_now || die "the bridge isn't paused; nothing was changed"

WORK=$(mktemp -d "$BACKUPS/upgrade-bridge-$(date +%Y%m%d-%H%M%S).XXXX")
log "backing up to $WORK"
cp -p "$HOME_DIR/bin/$CHAIN" "$WORK/coordinator"
cp -p "$SIGNER_BIN" "$WORK/signer"
[[ -f $COSIGNERS ]] && cp -p "$COSIGNERS" "$WORK/cosigners.json"
declare -A LOGFILE
for unit in "${SIGNERS[@]}"; do
  logfile=$(sed -nE 's/.* -log ([^ ;]+).*/\1/p' <<<"$(systemctl show -p ExecStart --value "$unit")")
  [[ -n $logfile ]] || die "$unit: can't read its -log"
  LOGFILE[$unit]=$logfile
  if [[ -f $logfile ]]; then cp -p "$logfile" "$WORK/$unit.signing-log.json"; fi
done

ok=0
cleanup() {
  [[ $ok == 1 ]] && return
  echo "upgrade-bridge: failed; putting the previous binaries, logs and cosigners.json back; the bridge stays paused" >&2
  systemctl stop "${COORD[@]}" "${SIGNERS[@]}" 2>/dev/null || true
  cp -p "$WORK/coordinator" "$HOME_DIR/bin/$CHAIN"
  cp -p "$WORK/signer" "$SIGNER_BIN"
  [[ -f $WORK/cosigners.json ]] && cp -p "$WORK/cosigners.json" "$COSIGNERS"
  for unit in "${SIGNERS[@]}"; do
    if [[ -f $WORK/$unit.signing-log.json ]]; then
      cp -p "$WORK/$unit.signing-log.json" "${LOGFILE[$unit]}"
    else
      rm -f "${LOGFILE[$unit]}" # one this run created
    fi
  done
  paused_now || as_user "$HOME_DIR/bin/$CHAIN" pause -signers "$SET" -reason "$REASON" || true
  systemctl start "${SIGNERS[@]}" "${COORD[@]}" || true
  echo "upgrade-bridge: restored from $WORK" >&2
}
trap cleanup EXIT

log "stopping"
systemctl stop "${COORD[@]}"
systemctl stop "${SIGNERS[@]}"

log "installing"
put() { # owner, source, destination
  if [[ -n ${UPGRADE_TEST:-} ]]; then cp "$2" "$3"; else install -o "$1" -g "$1" -m 755 "$2" "$3"; fi
}
put "$CHAIN" "$NEW" "$HOME_DIR/bin/$CHAIN"
put root "$NEW" "$SIGNER_BIN"
rm -f "$NEW"

log "signing logs"
for unit in "${SIGNERS[@]}"; do
  start=$(systemctl show -p ExecStart --value "$unit")
  arg() { sed -nE "s/.* $1 ([^ ;]+).*/\1/p" <<<"$start"; }
  signers=$(arg -signers) key=$(arg -key-file) deposits=$(arg -deposits) logfile=${LOGFILE[$unit]}
  [[ -n $signers && -n $key ]] || die "$unit: can't read its -signers and -key-file"
  steps=(check)
  if [[ ! -f $logfile ]]; then
    if listed "$INIT_LOGS" "$unit"; then
      steps=(init check)
    elif listed "$ADOPT_LOGS" "$unit"; then
      steps=(adopt check)
    else
      die "$unit has no signing log at $logfile. Restore it from a backup; or, only if this key never signed, run again with INIT_LOGS=\"$unit\"; or, only if it signed only before separate signers existed, ADOPT_LOGS=\"$unit\""
    fi
  fi
  for step in "${steps[@]}"; do
    echo "$unit: signer-log $step"
    runuser -u "$unit" -- bash -c 'set -a; . "$1"; set +a; shift; exec "$@"' _ "$VARLIB/$unit/signer.env" \
      "$SIGNER_BIN" signer-log "$step" -signers "$signers" -key-file "$key" -log "$logfile" ${deposits:+-deposits "$deposits"}
  done
done

log "cosigners.json"
# The coordinator now names the signer each request is for, so it needs
# every signer's public key. Sets assembled before that list only URLs:
# fill each key in from the signer's own card, matched by its listen
# address, and check every key is in the signer set and matches its card.
if [[ -f $COSIGNERS ]]; then
  pairs=()
  for unit in "${SIGNERS[@]}"; do
    listen=$(listen_of "$unit")
    card=$VARLIB/$unit/card.json
    [[ -n $listen && -f $card ]] && pairs+=("$listen" "$card")
  done
  python3 - "$COSIGNERS" "$SET" "${pairs[@]}" <<'PY'
import json, os, sys, tempfile
path, set_path, rest = sys.argv[1], sys.argv[2], sys.argv[3:]
cards = {rest[i]: json.load(open(rest[i + 1]))["publicKey"].lower() for i in range(0, len(rest), 2)}
signer_set = json.load(open(set_path))
known = {k.lower() for k in signer_set.get("publicKeys", [])}
for prev in signer_set.get("previous", []):
    known |= {k.lower() for k in prev.get("publicKeys", [])}
signers = json.load(open(path))
for s in signers:
    host = s["url"].split("://", 1)[-1].rstrip("/")
    if not s.get("publicKey"):
        if host not in cards:
            sys.exit(f"cosigners.json: no signer card for {s['url']}; add its publicKey by hand")
        s["publicKey"] = cards[host]
    key = s["publicKey"].lower()
    if key not in known:
        sys.exit(f"cosigners.json: {s['url']}'s key {key} is not in the signer set")
    if host in cards and cards[host] != key:
        sys.exit(f"cosigners.json: {s['url']}'s key doesn't match the card of the signer listening there")
    print(s["url"], "->", key[:16] + "...")
fd, tmp = tempfile.mkstemp(dir=os.path.dirname(path), prefix=".cosigners-")
with os.fdopen(fd, "w") as f:
    json.dump(signers, f, indent=2)
st = os.stat(path)
os.chown(tmp, st.st_uid, st.st_gid)
os.chmod(tmp, st.st_mode & 0o777)
os.replace(tmp, path)
PY
fi

log "contributions.txt"
# Historical top-ups that count as operating capital: every signer reads
# the same list as the coordinator (RUNBOOK.md, "Operating capital").
CONTRIB=$(dirname "$SET")/contributions.txt
for unit in "${SIGNERS[@]}"; do
  dest=$VARLIB/$unit/contributions.txt
  if [[ -f $CONTRIB ]]; then
    cp "$CONTRIB" "$dest"
    [[ -n ${UPGRADE_TEST:-} ]] || chown "$unit:$unit" "$dest"
    chmod 644 "$dest"
    echo "$unit: $(grep -cv '^[[:space:]]*\(#\|$\)' "$dest" || true) listed"
  fi
done

log "starting signers"
since=$(date '+%Y-%m-%d %H:%M:%S')
systemctl start "${SIGNERS[@]}"
sleep 5
for unit in "${SIGNERS[@]}"; do
  systemctl is-active --quiet "$unit" || die "$unit didn't start: journalctl -u $unit"
  if journalctl -u "$unit" --since "$since" --no-pager 2>/dev/null | grep -qi quarantin; then
    die "$unit reports a quarantine: journalctl -u $unit"
  fi
done

log "starting the bridge"
systemctl start "${COORD[@]}"
sleep 20
for unit in "${COORD[@]}"; do
  systemctl is-active --quiet "$unit" || die "$unit didn't start: journalctl -u $unit"
done
health=$(curl -s --max-time 30 "http://$WEB/api/health") || die "the web server doesn't answer"
python3 - "$health" <<'PY' || die "the peg check fails after the upgrade: $health"
import json, sys
d = json.loads(sys.argv[1])
peg = [c for c in d.get("checks", []) if c.get("name") == "peg"]
sys.exit(0 if peg and peg[0].get("ok") else 1)
PY
ok=1
trap - EXIT

if [[ $RESUME == 1 && $WAS_PAUSED == 0 ]]; then
  log "resuming"
  as_user "$HOME_DIR/bin/$CHAIN" resume -signers "$SET"
else
  echo
  echo "upgrade-bridge: done; the bridge is still paused. Read the checks:"
  echo "  curl -s http://$WEB/api/health"
  echo "then resume:"
  echo "  runuser -u $CHAIN -- $HOME_DIR/bin/$CHAIN resume -signers $SET"
fi
echo "Backups (old binaries, logs, cosigners.json) are in $WORK."
