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
#   3. stop the bridge, web wallet and monitor, so nothing is proposed;
#      pause every signer with its own pause file, so nothing is signed
#      even before it stops; then stop every signer. Only then, with
#      nothing able to sign, back up the binaries, cosigners.json, the
#      contribution lists and every signer's signing log;
#   4. install the new binary for the coordinator and the signers;
#   5. give every signer the coordinator's contributions.txt (outputs,
#      TXID:VOUT), next to the signer set it runs with, and check they all
#      read the same list;
#   6. check every signer's signing log (it must exist, match the chains,
#      and not be quarantined). A missing log stops the upgrade: a signer
#      that has signed must have its log restored from a backup. Only a
#      signer named in INIT_LOGS (its key never signed) gets a new empty
#      log, and only one in ADOPT_LOGS (it signed only before separate
#      signers existed, so all its signatures are on the chains) gets one
#      rebuilt from the chains;
#   7. set each signer's empty anchors to the tips of the anchor lines, as
#      its own nodes see them, and check every signer's anchors are the
#      tips the coordinator sees: a signer on a line of its own could sign
#      a second payout beside one the others signed (RUNBOOK.md, "Anchor
#      lineage");
#   8. fill each signer's public key into cosigners.json from its own card
#      where missing, and check every key there is in the signer set;
#   9. start the signers (still paused), then the bridge, web wallet and
#      monitor, and check the peg's audit is solvent.
#
# It never resumes unless RESUME=1, the bridge wasn't already paused
# before the upgrade, and every health check but the pause passes; then it
# resumes the bridge and each signer it paused. Otherwise read the checks,
# then resume the bridge and each signer (the commands are printed).
#
# On any failure after step 2, the previous binaries, cosigners.json and
# contribution lists go back, everything is started again, and the bridge
# and signers stay paused. Signing logs never go back: a log only grows,
# and an older copy over a newer one would forget a signature. A log this
# run created is removed only if nothing changed it since; any other log is
# left as it is.
set -euo pipefail

CHAIN=dogevm
COMMIT=${1:?usage: deploy/upgrade-bridge.sh COMMIT}
BRANCH=${BRANCH:-dogecoin}
HOME_DIR=${HOME_DIR:-/opt/$CHAIN}
STATE=${STATE:-/var/lib/metal-main}
VARLIB=${VARLIB:-/var/lib}
BACKUPS=${BACKUPS:-/root}
SET=${SET:-$STATE/secrets/separate-signers/signers.json}
BRIDGE_ENV=${BRIDGE_ENV:-$STATE/secrets/bridge.env}
SIGNER_BIN=${SIGNER_BIN:-/usr/local/lib/$CHAIN/$CHAIN}
INIT_LOGS=${INIT_LOGS:-}
ADOPT_LOGS=${ADOPT_LOGS:-}
RESUME=${RESUME:-0}
REASON=${REASON:-"Upgrading the bridge software. Funds are safe; deposits and withdrawals resume shortly."}

log() { printf '\n== %s\n' "$*"; }
die() { echo "upgrade-bridge: $*" >&2; exit 1; }
as_user() { runuser -u "$CHAIN" -- env HOME="$HOME_DIR" PATH="/usr/local/go/bin:$PATH" GOTOOLCHAIN=local "$@"; }
# as_signer UNIT CMD...: runs CMD as a signer's user, with its node settings.
as_signer() {
  local unit=$1
  shift
  runuser -u "$unit" -- bash -c 'set -a; . "$1"; set +a; shift; exec "$@"' _ "$VARLIB/$unit/signer.env" "$@"
}
# as_coordinator CMD...: runs CMD as the bridge's user, with its node settings.
as_coordinator() {
  runuser -u "$CHAIN" -- bash -c 'set -a; . "$1"; set +a; shift; exec "$@"' _ "$BRIDGE_ENV" "$@"
}
listed() { [[ " $1 " == *" $2 "* ]]; }
paused_at() { [[ -f $(dirname "$1")/paused.json ]]; }
start_of() { systemctl show -p ExecStart --value "$1"; }
flag_of() { sed -nE "s/.* $2 ([^ ;]+).*/\1/p" <<<"$(start_of "$1")"; }
hash_of() {
  if [[ ! -f $1 ]]; then echo none
  elif command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}
# The contribution list's outputs, normalised: lower case, sorted, no
# comments or blank lines.
outputs_of() { if [[ -f $1 ]]; then grep -v '^[[:space:]]*\(#\|$\)' "$1" | awk '{print tolower($1)}' | sort -u; fi; }

[[ $(id -u) == 0 || -n ${UPGRADE_TEST:-} ]] || die "run as root"
[[ -f $SET ]] || die "no signer set at $SET"
COSIGNERS=$(dirname "$SET")/cosigners.json
CONTRIB=$(dirname "$SET")/contributions.txt
mapfile -t SIGNERS < <(systemctl list-unit-files --no-legend "$CHAIN-signer-*.service" | awk '{print $1}' | sed 's/\.service$//' | sort -V)
[[ ${#SIGNERS[@]} -ge 1 ]] || die "no $CHAIN-signer-* units"
COORD=("$CHAIN-bridge-main" "$CHAIN-web-main" "$CHAIN-monitor-main")
WEB=$(flag_of "$CHAIN-web-main" -listen)
COORD_DEPOSITS=$(flag_of "$CHAIN-bridge-main" -deposits)

# Each signer's own files, from its unit: the set it runs with (its pause
# file and contribution list sit beside it), its key and its log.
declare -A SSET SKEY SLOG SDEPOSITS
for unit in "${SIGNERS[@]}"; do
  SSET[$unit]=$(flag_of "$unit" -signers)
  SKEY[$unit]=$(flag_of "$unit" -key-file)
  SLOG[$unit]=$(flag_of "$unit" -log)
  SDEPOSITS[$unit]=$(flag_of "$unit" -deposits)
  [[ -n ${SSET[$unit]} && -n ${SKEY[$unit]} && -n ${SLOG[$unit]} ]] || die "$unit: can't read its -signers, -key-file and -log"
done

if [[ -f $CONTRIB ]]; then
  bad=$(grep -v '^[[:space:]]*\(#\|$\)' "$CONTRIB" | awk '{print $1}' | grep -viE '^[0-9a-f]{64}:[0-9]+$' || true)
  [[ -z $bad ]] || die "$CONTRIB must list outputs, TXID:VOUT, one per line; not: $bad"
fi

log "building $CHAIN $COMMIT"
(
  cd "$HOME_DIR/src"
  as_user git fetch -q origin "$BRANCH"
  as_user git checkout -q --detach "$COMMIT"
  as_user git log -1 --format='%H %s'
  as_user go build -o "$HOME_DIR/bin/$CHAIN.new" "./cmd/$CHAIN"
)
NEW=$HOME_DIR/bin/$CHAIN.new

log "pausing the bridge"
WAS_PAUSED=0
if paused_at "$SET"; then WAS_PAUSED=1; echo "already paused: it stays paused afterwards"; fi
if [[ $WAS_PAUSED == 0 ]]; then
  as_user "$HOME_DIR/bin/$CHAIN" pause -signers "$SET" -reason "$REASON"
fi
paused_at "$SET" || die "the bridge isn't paused; nothing was changed"

# From here on, a failure puts things back (cleanup).
WORK=$(mktemp -d "$BACKUPS/upgrade-bridge-$(date +%Y%m%d-%H%M%S).XXXX")
declare -A PAUSED_BY_US LOG_BEFORE LOG_MADE
backed_up=0
ok=0
cleanup() {
  [[ $ok == 1 ]] && return
  echo "upgrade-bridge: failed; the bridge and signers stay paused" >&2
  systemctl stop "${COORD[@]}" "${SIGNERS[@]}" 2>/dev/null || true
  if [[ $backed_up == 1 ]]; then
    echo "upgrade-bridge: putting the previous binaries, cosigners.json and contribution lists back" >&2
    cp -p "$WORK/coordinator" "$HOME_DIR/bin/$CHAIN"
    cp -p "$WORK/signer" "$SIGNER_BIN"
    [[ -f $WORK/cosigners.json ]] && cp -p "$WORK/cosigners.json" "$COSIGNERS"
    restore_contrib "$WORK/coordinator.contributions.txt" "$CONTRIB"
    for unit in "${SIGNERS[@]}"; do
      restore_contrib "$WORK/$unit.contributions.txt" "$(dirname "${SSET[$unit]}")/contributions.txt"
      # Never an older log over a newer one. A log this run made, which
      # nothing changed since, goes; anything else stays as it is.
      logfile=${SLOG[$unit]}
      now=$(hash_of "$logfile")
      made=${LOG_MADE[$unit]:-}
      if [[ ${LOG_BEFORE[$unit]} == none && $now != none && $now == "$made" ]]; then
        rm -f "$logfile"
      elif [[ $now != "${LOG_BEFORE[$unit]}" && $now != "$made" ]]; then
        echo "upgrade-bridge: WARNING: $unit's signing log changed outside this run's own steps; it is kept as it is (a log is never rolled back). Find out what it signed before resuming $unit." >&2
      fi
    done
  fi
  paused_at "$SET" || as_user "$HOME_DIR/bin/$CHAIN" pause -signers "$SET" -reason "$REASON" || true
  systemctl start "${SIGNERS[@]}" "${COORD[@]}" || true
  echo "upgrade-bridge: see $WORK" >&2
}
restore_contrib() { # backup, destination
  if [[ -f $1 ]]; then cp -p "$1" "$2"; elif [[ -f $1.absent ]]; then rm -f "$2"; fi
}
trap cleanup EXIT

log "stopping the bridge, web wallet and monitor"
systemctl stop "${COORD[@]}" || die "couldn't stop ${COORD[*]}"

log "pausing and stopping the signers"
for unit in "${SIGNERS[@]}"; do
  if paused_at "${SSET[$unit]}"; then
    PAUSED_BY_US[$unit]=0
    echo "$unit: already paused"
  else
    as_signer "$unit" "$SIGNER_BIN" pause -signers "${SSET[$unit]}" -reason "$REASON" >/dev/null
    PAUSED_BY_US[$unit]=1
  fi
  paused_at "${SSET[$unit]}" || die "$unit isn't paused"
done
systemctl stop "${SIGNERS[@]}" || die "couldn't stop the signers"
for unit in "${SIGNERS[@]}"; do
  ! systemctl is-active --quiet "$unit" || die "$unit is still running"
done

log "backing up to $WORK (nothing can sign now)"
cp -p "$HOME_DIR/bin/$CHAIN" "$WORK/coordinator"
cp -p "$SIGNER_BIN" "$WORK/signer"
[[ -f $COSIGNERS ]] && cp -p "$COSIGNERS" "$WORK/cosigners.json"
if [[ -f $CONTRIB ]]; then cp -p "$CONTRIB" "$WORK/coordinator.contributions.txt"; else : >"$WORK/coordinator.contributions.txt.absent"; fi
for unit in "${SIGNERS[@]}"; do
  sc=$(dirname "${SSET[$unit]}")/contributions.txt
  if [[ -f $sc ]]; then cp -p "$sc" "$WORK/$unit.contributions.txt"; else : >"$WORK/$unit.contributions.txt.absent"; fi
  logfile=${SLOG[$unit]}
  LOG_BEFORE[$unit]=$(hash_of "$logfile")
  [[ -f $logfile ]] && cp -p "$logfile" "$WORK/$unit.signing-log.json"
done
backed_up=1

log "installing"
put() { # owner, source, destination
  if [[ -n ${UPGRADE_TEST:-} ]]; then cp "$2" "$3"; else install -o "$1" -g "$1" -m 755 "$2" "$3"; fi
}
put "$CHAIN" "$NEW" "$HOME_DIR/bin/$CHAIN"
put root "$NEW" "$SIGNER_BIN"
rm -f "$NEW"

log "contributions.txt"
# Historical top-ups that count as operating capital: every signer reads
# the same list as the coordinator (RUNBOOK.md, "Operating capital"),
# from beside the signer set it runs with.
want=$(outputs_of "$CONTRIB")
for unit in "${SIGNERS[@]}"; do
  dest=$(dirname "${SSET[$unit]}")/contributions.txt
  if [[ -f $CONTRIB ]]; then
    cp "$CONTRIB" "$dest"
    [[ -n ${UPGRADE_TEST:-} ]] || chown "$unit:$unit" "$dest"
    chmod 644 "$dest"
  else
    rm -f "$dest"
  fi
  [[ $(outputs_of "$dest") == "$want" ]] || die "$unit's contribution list differs from the coordinator's"
  echo "$unit: $(grep -c . <<<"$want" || true) output(s) listed"
done

log "signing logs"
for unit in "${SIGNERS[@]}"; do
  logfile=${SLOG[$unit]} deposits=${SDEPOSITS[$unit]}
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
    as_signer "$unit" "$SIGNER_BIN" signer-log "$step" -signers "${SSET[$unit]}" -key-file "${SKEY[$unit]}" -log "$logfile" ${deposits:+-deposits "$deposits"}
    LOG_MADE[$unit]=$(hash_of "$logfile") # as this run left it
  done
done

log "anchors"
# Every signer's anchors must be the tips of the lines the coordinator
# sees: set the empty ones from each signer's own nodes, then compare.
tips=$(as_coordinator "$HOME_DIR/bin/$CHAIN" signer-log anchors -signers "$SET" ${COORD_DEPOSITS:+-deposits "$COORD_DEPOSITS"})
echo "coordinator: $tips"
for unit in "${SIGNERS[@]}"; do
  deposits=${SDEPOSITS[$unit]}
  got=$(as_signer "$unit" "$SIGNER_BIN" signer-log anchors -set -signers "${SSET[$unit]}" -key-file "${SKEY[$unit]}" -log "${SLOG[$unit]}" ${deposits:+-deposits "$deposits"})
  python3 - "$unit" "$tips" "$got" <<'PY' || die "$unit's anchors aren't the coordinator's tips; stopped (RUNBOOK.md, \"Anchor lineage\")"
import json, sys
unit, coord, signer = sys.argv[1], json.loads(sys.argv[2])["tips"], json.loads(sys.argv[3])
if not coord:
    sys.exit(f"{unit}: the coordinator sees no anchor line yet")
anchors = signer.get("anchors") or {}
for chain, tip in coord.items():
    if signer.get("tips", {}).get(chain) != tip:
        sys.exit(f"{unit}: its nodes see the {chain} tip at {signer.get('tips', {}).get(chain)}, the coordinator's at {tip}")
    if anchors.get(chain) != tip:
        print(f"{unit}: its {chain} anchor {anchors.get(chain)} is behind the tip {tip} (it follows the line to it)")
print(f"{unit}: anchors agree")
PY
  LOG_MADE[$unit]=$(hash_of "${SLOG[$unit]}") # as this run left it
done

log "cosigners.json"
# The coordinator now names the signer each request is for, so it needs
# every signer's public key. Sets assembled before that list only URLs:
# fill each key in from the signer's own card, matched by its listen
# address, and check every key is in the signer set and matches its card.
if [[ -f $COSIGNERS ]]; then
  pairs=()
  for unit in "${SIGNERS[@]}"; do
    listen=$(flag_of "$unit" -listen)
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

log "starting the signers (paused)"
since=$(date '+%Y-%m-%d %H:%M:%S')
systemctl start "${SIGNERS[@]}" || die "couldn't start the signers"
sleep 5
for unit in "${SIGNERS[@]}"; do
  systemctl is-active --quiet "$unit" || die "$unit didn't start: journalctl -u $unit"
  if journalctl -u "$unit" --since "$since" --no-pager 2>/dev/null | grep -qi quarantin; then
    die "$unit reports a quarantine: journalctl -u $unit"
  fi
done

log "starting the bridge"
systemctl start "${COORD[@]}" || die "couldn't start ${COORD[*]}"
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

# Every check but the pause itself (which fails while paused, as it should).
failing=$(python3 - "$health" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
print(", ".join(f"{c.get('name')}: {c.get('detail')}" for c in d.get("checks", []) if c.get("name") != "pause" and not c.get("ok")))
PY
)

resume_cmds() {
  echo "  runuser -u $CHAIN -- $HOME_DIR/bin/$CHAIN resume -signers $SET"
  for unit in "${SIGNERS[@]}"; do
    echo "  runuser -u $unit -- $SIGNER_BIN resume -signers ${SSET[$unit]}"
  done
}
if [[ $RESUME == 1 && $WAS_PAUSED == 0 && -z $failing ]]; then
  log "resuming"
  for unit in "${SIGNERS[@]}"; do
    [[ ${PAUSED_BY_US[$unit]} == 1 ]] && as_signer "$unit" "$SIGNER_BIN" resume -signers "${SSET[$unit]}" >/dev/null
  done
  as_user "$HOME_DIR/bin/$CHAIN" resume -signers "$SET"
else
  echo
  [[ $RESUME == 1 && -n $failing ]] && echo "upgrade-bridge: not resuming: failing checks: $failing"
  echo "upgrade-bridge: done; the bridge and its signers are still paused. Read the checks:"
  echo "  curl -s http://$WEB/api/health"
  echo "then resume the signers and the bridge:"
  resume_cmds
fi
echo "Backups (old binaries, logs, cosigners.json, contribution lists) are in $WORK."
