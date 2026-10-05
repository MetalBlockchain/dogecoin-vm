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
#   6. compare coverage: the coordinator and every signer must have the same
#      registry and see the same unsettled coins of the set on both chains,
#      or nothing goes further (a cutover records only what its node sees);
#      then check every signer's signing log (it must exist, match the chains,
#      and not be quarantined). A missing log stops the upgrade: a signer
#      that has signed must have its log restored from a backup. Only a
#      signer named in INIT_LOGS (its key never signed) gets a new empty
#      log, and only one in ADOPT_LOGS (it signed only before separate
#      signers existed, so all its signatures are on the chains) gets one
#      rebuilt from the chains;
#   7. start a cutover for every current-set signer whose log is unproven
#      (adopted, or from software before the cutover rule, whatever its
#      anchors), and check its nodes see the coordinator's tips; for a
#      proven log, set its empty anchors and check they lead, along
#      confirmed spends, to those tips (RUNBOOK.md, "Cutover", "Anchor
#      lineage");
#   8. fill each signer's public key into cosigners.json from its own card
#      where missing, and check every key there is in the signer set;
#   9. start the signers (still paused) and the web wallet and monitor;
#      the bridge stays stopped. From here nothing is rolled back: the new
#      signers may sign a fence, and the old binary knows nothing of the
#      cutover gate;
#  10. while any current-set signer is unproven or has coins left in a
#      cutover (later epochs included), send fences on both chains with the
#      bridge unit's own flags (signers sign them while paused, and nothing
#      else), every FENCE_WAIT_POLL seconds (default 60), for up to
#      FENCE_WAIT minutes (default 360; 0: one round). If that time runs
#      out, stop with the bridge stopped and everything paused (exit 1);
#  11. start the bridge, take fresh health, and check the peg is solvent.
#
# It resumes only if every cutover is done, RESUME=1, the bridge wasn't
# already paused before the upgrade, and every health check but the pause
# is present and passing; then it resumes the bridge and each signer it
# paused. Otherwise read the checks, then resume the bridge and each signer
# (the commands are printed).
#
# On any failure after step 2 and before step 9, the previous binaries, cosigners.json and
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

log "coverage"
# Before any log is adopted or a cutover records anything (round 7, W7):
# every node must cover the same coins. A cutover records what its own
# wallet and registry list, so a deposit address one signer never
# registered, or a payment one wallet never saw, would go unrecorded there,
# and a signature given before the upgrade could spend it beside a new one.
# The coordinator and every signer print their coverage (registry hash and
# the set's unsettled coins on both chains); any difference stops here,
# with nothing started (RUNBOOK.md, "First upgrade of the live bridge").
want=$(as_coordinator "$HOME_DIR/bin/$CHAIN" coverage -signers "$SET" ${COORD_DEPOSITS:+-deposits "$COORD_DEPOSITS"}) || die "coverage on the coordinator failed"
echo "coordinator: $(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print(d["registered"], "registered,", {c: len(v) for c, v in d["coins"].items()}, "coins")' "$want")"
for unit in "${SIGNERS[@]}"; do
  got=$(as_signer "$unit" "$SIGNER_BIN" coverage -signers "${SSET[$unit]}" ${SDEPOSITS[$unit]:+-deposits "${SDEPOSITS[$unit]}"}) || die "coverage on $unit failed"
  diff=$(python3 - "$want" "$got" <<'PY'
import json, sys
want, got = json.loads(sys.argv[1]), json.loads(sys.argv[2])
out = []
if want["registry"] != got["registry"]:
    out.append(f"registry differs ({got['registered']} registered vs the coordinator's {want['registered']})")
for chain in sorted(set(want["coins"]) | set(got["coins"])):
    w, g = set(want["coins"].get(chain) or []), set(got["coins"].get(chain) or [])
    if g - w: out.append(f"{chain}: only here: {sorted(g - w)[:5]}")
    if w - g: out.append(f"{chain}: missing here: {sorted(w - g)[:5]}")
print("; ".join(out))
PY
)
  [[ -z $diff ]] || die "$unit doesn't cover what the coordinator covers: $diff. Make every registry and wallet the same (register the same addresses, rescan) and run again; nothing was started (RUNBOOK.md, \"First upgrade of the live bridge\")"
  echo "$unit: same coverage"
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

log "cutover and anchors"
# Every current-set signer whose log is unproven (adopted, or written by
# software before the cutover rule, whatever its anchors) starts a cutover
# on both chains: it signs only fences until every coin a signature kept
# back could spend is spent deep (RUNBOOK.md, "Cutover"). Its nodes must
# see the coordinator's tips. A proven log (new, or fenced) keeps its
# anchors, which must lead to those tips.
tips=$(as_coordinator "$HOME_DIR/bin/$CHAIN" signer-log anchors -signers "$SET" ${COORD_DEPOSITS:+-deposits "$COORD_DEPOSITS"})
echo "coordinator: $tips"
signer_log() { # unit step [args]: signer-log as that signer, with its own files
  local unit=$1 step=$2
  shift 2
  as_signer "$unit" "$SIGNER_BIN" signer-log "$step" -signers "${SSET[$unit]}" -key-file "${SKEY[$unit]}" -log "${SLOG[$unit]}" \
    ${SDEPOSITS[$unit]:+-deposits "${SDEPOSITS[$unit]}"} "$@"
}
in_cutover() { python3 -c 'import json,sys; d=json.loads(sys.argv[1]); sys.exit(0 if any(d.get("cutoverLeft") or {}) or not d.get("history") else 1)' "$1"; }
for unit in "${SIGNERS[@]}"; do
  st=$(signer_log "$unit" status)
  if python3 -c 'import json,sys; sys.exit(0 if not json.loads(sys.argv[1]).get("history") and not json.loads(sys.argv[1]).get("cutoverLeft") else 1)' "$st"; then
    echo "$unit: unproven log: starting a cutover"
    st=$(signer_log "$unit" cutover)
  fi
  if in_cutover "$st"; then
    python3 - "$unit" "$tips" "$st" <<'PY' || die "$unit's nodes don't see the coordinator's tips; stopped (RUNBOOK.md, \"Anchor lineage\")"
import json, sys
unit, coord, st = sys.argv[1], json.loads(sys.argv[2])["tips"], json.loads(sys.argv[3])
if not coord:
    sys.exit(f"{unit}: the coordinator sees no anchor line yet")
for chain, tip in coord.items():
    if (st.get("lineageTips") or {}).get(chain) != tip:
        sys.exit(f"{unit}: its nodes see the {chain} tip at {(st.get('lineageTips') or {}).get(chain)}, the coordinator's at {tip}")
left = {c: len(v) for c, v in (st.get("cutoverLeft") or {}).items()}
print(f"{unit}: in cutover {st.get('cutoverEpoch')}; coins left per chain: {left}")
PY
  else
    got=$(signer_log "$unit" anchors -set)
    python3 - "$unit" "$tips" "$got" <<'PY' || die "$unit's anchors aren't the coordinator's tips; stopped (RUNBOOK.md, \"Anchor lineage\")"
import json, sys
unit, coord, signer = sys.argv[1], json.loads(sys.argv[2])["tips"], json.loads(sys.argv[3])
if not coord:
    sys.exit(f"{unit}: the coordinator sees no anchor line yet")
walked = signer.get("walked") or {}
for chain, tip in coord.items():
    if signer.get("tips", {}).get(chain) != tip:
        sys.exit(f"{unit}: its nodes see the {chain} tip at {signer.get('tips', {}).get(chain)}, the coordinator's at {tip}")
    # Where the log's anchor leads along confirmed spends, not the anchor
    # itself: an anchor a confirmed step behind the tip is normal.
    if walked.get(chain) != tip:
        sys.exit(f"{unit}: its {chain} anchor leads to {walked.get(chain)}, not the coordinator's tip {tip}")
print(f"{unit}: anchors lead to the coordinator's tips")
PY
  fi
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

# The point of no return comes before any signer starts (round 7, W2):
# signers start one by one, and the first may sign a fence before a later
# one fails, which the old binary, knowing nothing of the cutover gate,
# must never take over from. So from here a failure rolls nothing back:
# the new binaries and every log stay, the bridge stays stopped and every
# signer paused, for the operator.
ok=1
trap - EXIT
forward_failure() {
  local code=$?
  [[ $code == 0 || ${DONE:-0} == 1 ]] && return
  echo "upgrade-bridge: failed after the point of no return: nothing is rolled back (the new binaries and every signing log stay)." >&2
  echo "upgrade-bridge: the bridge is stopped and every signer stays paused. Fix the cause and carry on by hand (RUNBOOK.md, \"Upgrading the bridge software\"); backups are in $WORK" >&2
  systemctl stop "$CHAIN-bridge-main" 2>/dev/null || true
  paused_at "$SET" || as_user "$HOME_DIR/bin/$CHAIN" pause -signers "$SET" -reason "$REASON" || true
}
trap forward_failure EXIT
log "starting the signers (paused)"
since=$(date '+%Y-%m-%d %H:%M:%S')
systemctl start "${SIGNERS[@]}" || die "couldn't start every signer"
sleep 5
for unit in "${SIGNERS[@]}"; do
  systemctl is-active --quiet "$unit" || die "$unit didn't start: journalctl -u $unit"
  if journalctl -u "$unit" --since "$since" --no-pager 2>/dev/null | grep -qi quarantin; then
    die "$unit reports a quarantine: journalctl -u $unit"
  fi
done

# The web wallet and monitor run (the site shows the pause); the bridge
# stays stopped until every cutover is done.
log "starting the web wallet and monitor (the bridge stays stopped)"
systemctl start "$CHAIN-web-main" "$CHAIN-monitor-main" || die "couldn't start the web wallet or monitor"

resume_cmds() {
  echo "  runuser -u $CHAIN -- $HOME_DIR/bin/$CHAIN resume -signers $SET"
  for unit in "${SIGNERS[@]}"; do
    echo "  runuser -u $unit -- $SIGNER_BIN resume -signers ${SSET[$unit]}"
  done
}
# The coordinator's fence runs with exactly the bridge unit's own command
# line (its registry, cosigners, coordinator key and policy flags, which
# bridge.env alone doesn't give), plus -fence (round 5, U9).
BRIDGE_ARGV=$(start_of "$CHAIN-bridge-main")
if [[ $BRIDGE_ARGV == *"argv[]="* ]]; then
  BRIDGE_ARGV=${BRIDGE_ARGV#*argv[]=}
  BRIDGE_ARGV=${BRIDGE_ARGV%% ;*}
fi
read -r -a BRIDGE_ARGS <<<"$BRIDGE_ARGV"
[[ ${BRIDGE_ARGS[1]:-} == bridge ]] || die "can't read $CHAIN-bridge-main's command line: $BRIDGE_ARGV"
send_fence() { # chain
  as_coordinator "$HOME_DIR/bin/$CHAIN" "${BRIDGE_ARGS[@]:1}" -fence "$1"
}
fence_cmds() {
  for chain in dogecoin dogecoinvm; do
    echo "  runuser -u $CHAIN -- bash -c 'set -a; . $BRIDGE_ENV; set +a; exec $HOME_DIR/bin/$CHAIN ${BRIDGE_ARGS[*]:1} -fence $chain'"
  done
}
# cutover_left: every current-set signer not yet done (history unproven, or
# coins left on a chain, later epochs included), with its coins left.
cutover_left() {
  local unit st
  for unit in "${SIGNERS[@]}"; do
    st=$(signer_log "$unit" status) || { echo "$unit: status failed"; continue; }
    if in_cutover "$st"; then
      echo "$unit: $(python3 -c 'import json,sys; d=json.loads(sys.argv[1]); print("history=%r cutoverLeft=%s" % (d.get("history") or "", {c: len(v) for c, v in (d.get("cutoverLeft") or {}).items()}))' "$st")"
    fi
  done
}

FENCE_WAIT=${FENCE_WAIT:-360}       # minutes; 0: one round of fences, no waiting
FENCE_WAIT_POLL=${FENCE_WAIT_POLL:-60} # seconds between rounds
left=$(cutover_left)
if [[ -n $left ]]; then
  # Fences pay no one, so the signers sign them while paused, and nothing
  # else. A Dogecoin fence needs rootDepth confirmations (about 20
  # minutes); a set with more coins than one fence holds takes several in
  # turn, and a stuck one is replaced after -fence-bump-after.
  log "cutover: sending fences until every signer is done (FENCE_WAIT=${FENCE_WAIT}m)"
  deadline=$(( $(date +%s) + FENCE_WAIT * 60 ))
  while :; do
    for chain in dogecoin dogecoinvm; do
      send_fence "$chain" || echo "($chain: no fence now; see above)"
    done
    left=$(cutover_left)
    [[ -z $left ]] && break
    (( $(date +%s) < deadline )) || break
    sleep "$FENCE_WAIT_POLL"
  done
fi
if [[ -n $left ]]; then
  echo
  echo "upgrade-bridge: the cutover isn't finished. The bridge is stopped and every signer stays paused:"
  echo "$left"
  echo "Keep sending fences until every signer shows cutoverLeft {} AND history \"fenced\" (RUNBOOK.md, \"Cutover\"):"
  fence_cmds
  echo "check each signer with:"
  for unit in "${SIGNERS[@]}"; do
    echo "  runuser -u $unit -- bash -c 'set -a; . $VARLIB/$unit/signer.env; set +a; exec $SIGNER_BIN signer-log status -signers ${SSET[$unit]} -key-file ${SKEY[$unit]} -log ${SLOG[$unit]}'"
  done
  echo "Only then start the bridge (systemctl start $CHAIN-bridge-main), read the checks and resume (RUNBOOK.md, \"Upgrading the bridge software\")."
  echo "Backups (old binaries, logs, cosigners.json, contribution lists) are in $WORK."
  DONE=1 # an expected stop, not a failure: everything stays as it is
  exit 1
fi
log "every current-set signer is done with its cutover"

log "starting the bridge"
systemctl start "$CHAIN-bridge-main" || die "couldn't start $CHAIN-bridge-main"
sleep 20
for unit in "${COORD[@]}"; do
  systemctl is-active --quiet "$unit" || die "$unit didn't start: journalctl -u $unit"
done
# Fresh health, taken now: every expected check present, and none failing
# but the pause.
health=$(curl -s --max-time 30 "http://$WEB/api/health") || die "the web server doesn't answer"
failing=$(python3 - "$health" <<'PY'
import json, sys
d = json.loads(sys.argv[1])
checks = {c.get("name"): c for c in d.get("checks", [])}
problems = [f"{name}: missing" for name in ("pause", "peg", "bridge", "dogecoin", "dogecoinvm") if name not in checks]
problems += [f"{n}: {c.get('detail')}" for n, c in checks.items() if n != "pause" and not c.get("ok")]
print(", ".join(problems))
PY
) || die "can't read the health checks: $health"
python3 - "$health" <<'PY' || die "the peg check fails after the upgrade: $health"
import json, sys
d = json.loads(sys.argv[1])
peg = [c for c in d.get("checks", []) if c.get("name") == "peg"]
sys.exit(0 if peg and peg[0].get("ok") else 1)
PY

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
DONE=1
