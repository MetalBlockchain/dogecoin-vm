#!/usr/bin/env bash
# Exercises deploy/upgrade-bridge.sh on a fake host: stub systemctl (which
# keeps each unit's state), runuser, journalctl, curl, git and go, and a
# stub dogevm binary that pauses, resumes, checks signing logs and reports
# anchors. Each signer has its own directory, with its own signer set,
# pause file and contribution list, as signer-setup join makes them. Run
# from the repo root, with bash 4 or later:
#
#   deploy/test/upgrade-bridge-test.sh      # BASH5=$(brew --prefix)/bin/bash on macOS
set -uo pipefail

SCRIPT=$(cd "$(dirname "$0")/.." && pwd)/upgrade-bridge.sh
pass=0 fail=0
ok() { echo "  ok: $*"; pass=$((pass + 1)); }
bad() { echo "  FAIL: $*"; fail=$((fail + 1)); }
expect() { if eval "$1"; then ok "$2"; else bad "$2"; fi; }

# The stub binary. VERSION is printed by "version"; pause/resume write and
# remove paused.json next to -signers (and log it); signer-log checks,
# creates and adopts -log, and "anchors" prints the tips ($TIPS, or
# $TIPS_<unit> for a signer whose nodes see another line), the log's
# anchors ($ANCHORS_<unit>, default the tips), where they lead
# ($WALKED_<unit>, default the tips); with -set it records them in the log.
# "status" reports a log unproven (history "") when NEEDS_FENCE is set or
# it was adopted, and in a cutover (coins left) once "cutover" (or adopt)
# started one, until FENCES_NEEDED (default 2) fences were sent. "bridge
# ... -fence CHAIN" logs the fence with its whole command line.
stub_bridge() { # path version
  cat >"$1" <<EOF
#!/usr/bin/env bash
cmd=\$1; shift
arg() { local want=\$1; shift; while [[ \$# -gt 0 ]]; do [[ \$1 == \$want ]] && { echo "\$2"; return; }; shift; done; }
has() { local want=\$1; shift; for a in "\$@"; do [[ \$a == \$want ]] && return 0; done; return 1; }
unit_of() { basename "\$(dirname "\$1")"; }
case \$cmd in
  version) echo "$2" ;;
  coverage) set=\$(arg -signers "\$@"); u=\$(unit_of "\$set"); v=COVERAGE_\${u//-/_}
    d='{"registry":"r1","registered":2,"coins":{"dogecoin":["aa:0"],"dogecoinvm":["bb:1"]}}'; echo "\${!v:-\$d}" ;;
  inventory) [[ -n \${INVENTORY_FAIL:-} ]] && { echo "\$INVENTORY_FAIL" >&2; exit 1; }
    if [[ -n \${INVENTORY_PROBLEM:-} ]]; then echo "{\"problems\":[\"\$INVENTORY_PROBLEM\"],\"ready\":false}"; echo "not ready" >&2; exit 1; fi
    echo '{"chains":{"dogecoin":{"coins":[{"outpoint":"aa:0"}]},"dogecoinvm":{"coins":[{"outpoint":"bb:1"}]}},"surplus":"9.00000000","needed":"2.00000000","problems":[],"ready":true}' ;;
  pause) set=\$(arg -signers "\$@"); [[ -n \${FAIL_PAUSE:-} && \$set == *\$FAIL_PAUSE* && -f "\$(dirname "\$set")/resumed-once" ]] && exit 1
    [[ -n \${FAIL_PAUSE_ALWAYS:-} && \$set == *\$FAIL_PAUSE_ALWAYS* ]] && exit 1
    echo '{"reason":"x"}' >"\$(dirname "\$set")/paused.json"; echo "pause \$(dirname "\$set")" >>"\$EVENTS" ;;
  resume) set=\$(arg -signers "\$@"); [[ -n \${FAIL_RESUME:-} && \$set == *\$FAIL_RESUME* ]] && exit 1
    rm -f "\$(dirname "\$set")/paused.json"; touch "\$(dirname "\$set")/resumed-once"; echo "resume \$(dirname "\$set")" >>"\$EVENTS" ;;
  signer-log)
    step=\$1; shift; log=\$(arg -log "\$@"); set=\$(arg -signers "\$@")
    tips=\${TIPS:-'{"dogecoin":"aa:1","dogecoinvm":"bb:1"}'}
    if [[ -n \$set ]]; then u=\$(unit_of "\$set"); v=TIPS_\${u//-/_}; tips=\${!v:-\$tips}; fi
    case \$step in
      init) [[ -f \$log ]] && { echo "exists" >&2; exit 1; }; echo '{"actions":{}}' >"\$log" ;;
      adopt) [[ -f \$log ]] && { echo "exists" >&2; exit 1; }; printf '{"actions":{}}\\nUNPROVEN\\nCUTOVER\\n' >"\$log" ;;
      cutover|status)
        [[ \$step == cutover ]] && echo CUTOVER >>"\$log"
        unproven=\${NEEDS_FENCE:-}; grep -q UNPROVEN "\$log" && unproven=1
        # STATUS_NOISE: as 8e91a6b-era logs did on the rehearsal, loading an
        # unproven log starts its cutover and says so on stdout.
        if [[ -n \${STATUS_NOISE:-} && -n \$unproven ]] && ! grep -q CUTOVER "\$log"; then
          echo "2026-10-08T22:53:00Z cutover 1 started"; echo CUTOVER >>"\$log"
        fi
        sent=\$(grep -c '^fence' "\$EVENTS" || true)
        if [[ -n \$unproven ]] && grep -q CUTOVER "\$log" && (( sent < \${FENCES_NEEDED:-2} )); then
          left=\${CUTOVER_LEFT:-'{"dogecoin":["aa:0"],"dogecoinvm":["bb:1"]}'}; echo "{\\"history\\":\\"\\",\\"cutoverEpoch\\":1,\\"cutoverLeft\\":\$left,\\"lineageTips\\":\$tips}"
        elif [[ -n \$unproven ]] && ! grep -q CUTOVER "\$log"; then
          echo "{\\"history\\":\\"\\",\\"cutoverLeft\\":{},\\"lineageTips\\":\$tips}"
        else
          echo "{\\"history\\":\\"fenced\\",\\"cutoverLeft\\":{},\\"lineageTips\\":\$tips}"
        fi ;;
      check) [[ -f \$log ]] || { echo "no log" >&2; exit 1; }; grep -q QUARANTINE "\$log" && { echo "quarantined" >&2; exit 1; }; true ;;
      anchors)
        if [[ -z \$(arg -key-file "\$@") ]]; then [[ -n \${ANCHORS_TIPS_STATUS:-} ]] && exit \$ANCHORS_TIPS_STATUS; echo "{\"tips\":\$tips}"; exit 0; fi
        has -set "\$@" && echo "ANCHORS \$tips" >>"\$log"
        anchors=\$tips; walked=\$tips; fence=null
        if [[ -n \$set ]]; then
          v=ANCHORS_\${u//-/_}; anchors=\${!v:-\$anchors}
          v=WALKED_\${u//-/_}; walked=\${!v:-\$walked}
        fi
        echo "{\"anchors\":\$anchors,\"tips\":\$tips,\"walked\":\$walked}" ;;
    esac ;;
  bridge) has -fence "\$@" && { echo "fence \$(arg -fence "\$@") \$*" >>"\$EVENTS"; echo '{"fenceTxid":"ff"}'; } ;;
esac
EOF
  chmod +x "$1"
}

setup() { # makes a fresh fake host in $T
  T=$(mktemp -d)
  export HOME_DIR=$T/opt STATE=$T/state VARLIB=$T/varlib BACKUPS=$T/backups SIGNER_BIN=$T/signerbin/dogevm UPGRADE_TEST=1
  export SET=$STATE/secrets/separate-signers/signers.json BRANCH=main EVENTS=$T/events BRIDGE_ENV=$STATE/secrets/bridge.env
  mkdir -p "$HOME_DIR/bin" "$HOME_DIR/src" "$(dirname "$SET")" "$BACKUPS" "$(dirname "$SIGNER_BIN")" "$T/bin" "$T/units" "$T/active"
  stub_bridge "$HOME_DIR/bin/dogevm" old
  stub_bridge "$SIGNER_BIN" old
  echo "DOGECOIN_NETWORK=mainnet" >"$BRIDGE_ENV"
  echo '{"required":2,"publicKeys":["02aa","02bb","02cc"]}' >"$SET"
  echo '[{"url":"http://127.0.0.1:9701"},{"url":"http://127.0.0.1:9702"},{"url":"http://127.0.0.1:9703"}]' >"$(dirname "$SET")/cosigners.json"
  local n key dir
  for n in 1 2 3; do
    key=$(sed -n "${n}p" <<<$'02aa\n02bb\n02cc')
    dir=$VARLIB/dogevm-signer-$n
    mkdir -p "$dir"
    cp "$SET" "$dir/signers.json"
    echo "DOGECOIN_NETWORK=mainnet" >"$dir/signer.env"
    echo "{\"publicKey\":\"$key\"}" >"$dir/card.json"
    echo '{"actions":{}}' >"$dir/signing-log.json"
    echo "/x/dogevm signer -signers $dir/signers.json -key-file $dir/signer.key -listen 127.0.0.1:970$n -log $dir/signing-log.json -deposits $dir/deposits.json -max-daily 1" >"$T/units/dogevm-signer-$n"
    touch "$T/active/dogevm-signer-$n"
  done
  echo "/x/dogevm serve -signers $SET -listen 127.0.0.1:8081" >"$T/units/dogevm-web-main"
  echo "/x/dogevm bridge -signers $SET -deposits $STATE/registry/deposits.json" >"$T/units/dogevm-bridge-main"
  echo "/x/dogevm monitor -signers $SET" >"$T/units/dogevm-monitor-main"
  touch "$T/active/dogevm-web-main" "$T/active/dogevm-bridge-main" "$T/active/dogevm-monitor-main"
  : >"$EVENTS"
  # systemctl keeps each unit's state in $T/active; FAIL_STOP and
  # FAIL_START name a unit that won't stop or start.
  cat >"$T/bin/systemctl" <<EOF
#!/usr/bin/env bash
cmd=\$1; shift
case \$cmd in
  list-unit-files) for n in 1 2 3; do echo "dogevm-signer-\$n.service enabled enabled"; done ;;
  show) unit=\${@: -1}; cat "$T/units/\${unit%.service}" ;;
  stop) for u in "\$@"; do
      [[ \${FAIL_STOP:-} == "\$u" ]] && { echo "stop \$u failed" >>"\$EVENTS"; exit 1; }
      [[ \${FAIL_STOP_RESUMED:-} == "\$u" && -f "$VARLIB/\$u/resumed-once" ]] && { echo "stop \$u failed" >>"\$EVENTS"; exit 1; }
      rm -f "$T/active/\$u"; echo "stop \$u" >>"\$EVENTS"
    done ;;
  start) for u in "\$@"; do
      [[ \${FAIL_START:-} == "\$u" ]] && { echo "start \$u failed" >>"\$EVENTS"; continue; }
      touch "$T/active/\$u"; echo "start \$u" >>"\$EVENTS"
      # LATE_WRITE: that signer's log changes once it runs again.
      if [[ \${LATE_WRITE:-} == "\$u" ]]; then echo SIGNED-LATE >>"$VARLIB/\$u/signing-log.json"; fi
    done ;;
  is-active) unit=\${@: -1}; [[ -f "$T/active/\$unit" ]] ;;
  kill) u=\${@: -1}
      [[ \${FAIL_KILL:-} == "\$u" ]] && { echo "kill \$u failed" >>"\$EVENTS"; exit 1; }
      rm -f "$T/active/\$u"; echo "kill \$u" >>"\$EVENTS" ;;
esac
EOF
  cat >"$T/bin/runuser" <<'EOF'
#!/usr/bin/env bash
shift 2; [[ $1 == -- ]] && shift
[[ $1 == env ]] && { shift; while [[ $1 == *=* ]]; do export "$1"; shift; done; }
exec "$@"
EOF
  cat >"$T/bin/journalctl" <<'EOF'
#!/usr/bin/env bash
[[ -n ${QUARANTINE_UNIT:-} && " $* " == *" $QUARANTINE_UNIT "* ]] && echo "signer quarantined"
exit 0
EOF
  cat >"$T/bin/curl" <<'EOF'
#!/usr/bin/env bash
if [[ -n ${HEALTH:-} ]]; then echo "$HEALTH"; else echo '{"checks":[{"name":"pause","ok":false},{"name":"peg","ok":true},{"name":"bridge","ok":true},{"name":"dogecoin","ok":true},{"name":"dogecoinvm","ok":true}]}'; fi
EOF
  cat >"$T/bin/git" <<'EOF'
#!/usr/bin/env bash
[[ $1 == log ]] && echo "abc123 a commit"
exit 0
EOF
  cat >"$T/bin/go" <<EOF
#!/usr/bin/env bash
while [[ \$# -gt 0 ]]; do [[ \$1 == -o ]] && out=\$2; shift; done
cat "$HOME_DIR/bin/dogevm" | sed 's/echo "old"/echo "new"/' >"\$out"; chmod +x "\$out"
EOF
  cat >"$T/bin/sleep" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
  chmod +x "$T"/bin/*
  export PATH=$T/bin:$PATH
}
# FENCE_WAIT=0: one round of fences (sleep is stubbed, so a wait would spin).
run() { FENCE_WAIT=${FENCE_WAIT:-0} "${BASH5:-bash}" "$SCRIPT" abc123 >"$T/out" 2>&1; echo $?; }
paused() { [[ -f $(dirname "$SET")/paused.json ]]; }
signer_paused() { [[ -f $VARLIB/dogevm-signer-$1/paused.json ]]; }
all_signers_paused() { signer_paused 1 && signer_paused 2 && signer_paused 3; }
version() { "$HOME_DIR/bin/dogevm" version; }
line_of() { grep -n -m1 -- "$1" "$EVENTS" | cut -d: -f1; }
# Every signer paused, then stopped, before anything was backed up: the
# backup's log copies are written after the last signer stopped.
quiet_before_backup() {
  local last_stop
  last_stop=$(grep -n '^stop dogevm-signer-' "$EVENTS" | tail -1 | cut -d: -f1)
  [[ -n $last_stop ]] && [[ $(line_of "pause $VARLIB/dogevm-signer-3") -lt $last_stop ]] &&
    [[ $(line_of "stop dogevm-bridge-main") -lt $(line_of "pause $VARLIB/dogevm-signer-1") ]]
}

echo "1. success: new binary, signers paused and stopped before the backup, anchors agree, still paused"
setup
code=$(run)
expect '[[ $code == 0 ]]' "exits 0 ($(tail -3 "$T/out" | tr '\n' ' '))"
expect '[[ $(version) == new && $("$SIGNER_BIN" version) == new ]]' "both binaries upgraded"
expect 'quiet_before_backup' "coordinator stopped, then every signer paused, then stopped"
expect 'paused && all_signers_paused' "bridge and signers left paused"
expect 'grep -q 02bb "$(dirname "$SET")/cosigners.json"' "public keys filled in"
expect 'grep -q "anchors lead to the coordinator.s tips" "$T/out"' "anchors compared"
expect 'grep -q "runuser -u dogevm-signer-2 .* resume -signers $VARLIB/dogevm-signer-2/signers.json" "$T/out"' "prints how to resume each signer"

echo "2. a missing signing log stops it and everything goes back"
setup
rm "$VARLIB/dogevm-signer-3/signing-log.json"
code=$(run)
expect '[[ $code != 0 ]]' "fails"
expect 'grep -q "has no signing log" "$T/out"' "says why"
expect '[[ $(version) == old && $("$SIGNER_BIN" version) == old ]]' "old binaries back"
expect 'paused && all_signers_paused' "still paused, signers too"
expect '[[ ! -f $VARLIB/dogevm-signer-3/signing-log.json ]]' "no log invented"

echo "3. INIT_LOGS names a signer that never signed; its anchors are set"
setup
rm "$VARLIB/dogevm-signer-3/signing-log.json"
code=$(INIT_LOGS=dogevm-signer-3 run)
expect '[[ $code == 0 ]]' "succeeds"
expect 'grep -q ANCHORS "$VARLIB/dogevm-signer-3/signing-log.json"' "a log made for it, with its anchors"

echo "4. a failure before the signers run rolls back; a log it made, unchanged since, goes"
setup
rm "$VARLIB/dogevm-signer-3/signing-log.json"
code=$(INIT_LOGS=dogevm-signer-3 TIPS_dogevm_signer_2='{"dogecoin":"ff:0","dogecoinvm":"bb:1"}' run)
expect '[[ $code != 0 ]]' "fails"
expect '[[ $(version) == old ]]' "old binary back"
expect '[[ ! -f $VARLIB/dogevm-signer-3/signing-log.json ]]' "the log it made is removed"
expect '! grep -q publicKey "$(dirname "$SET")/cosigners.json"' "cosigners.json restored"
expect 'paused && all_signers_paused' "still paused"
setup
code=$(HEALTH='{"checks":[{"name":"peg","ok":false}]}' run)
expect '[[ $code != 0 && $(version) == new ]] && paused && all_signers_paused' "an unhealthy peg once the signers run: no rollback (round 6, V7), everything paused"

echo "5. a pause already in place stays, even with RESUME=1"
setup
echo '{}' >"$(dirname "$SET")/paused.json"
code=$(RESUME=1 run)
expect '[[ $code == 0 ]]' "succeeds"
expect 'paused && all_signers_paused' "still paused"

echo "6. RESUME=1 resumes the bridge and the signers it paused, when every check passes"
setup
echo '{}' >"$VARLIB/dogevm-signer-3/paused.json" # its operator's own pause
code=$(RESUME=1 run)
expect '[[ $code == 0 ]] && ! paused' "bridge resumed"
expect '! signer_paused 1 && ! signer_paused 2' "the signers it paused resumed"
expect 'signer_paused 3' "a signer its operator paused stays paused"

echo "7. RESUME=1 doesn't resume while a check other than the peg fails"
setup
code=$(RESUME=1 HEALTH='{"checks":[{"name":"pause","ok":false},{"name":"peg","ok":true},{"name":"bridge","ok":true},{"name":"dogecoin","ok":true},{"name":"dogecoinvm","ok":false,"detail":"unhealthy"}]}' run)
expect '[[ $code == 0 ]]' "the upgrade itself stands"
expect 'paused && all_signers_paused' "nothing resumed"
expect 'grep -q "not resuming: failing checks: dogecoinvm" "$T/out"' "says why"

echo "8. a quarantined signer log stops it"
setup
echo QUARANTINE >>"$VARLIB/dogevm-signer-2/signing-log.json"
code=$(run)
expect '[[ $code != 0 && $(version) == old ]] && paused' "fails, old binary back, paused"

echo "9. a cosigners.json key outside the signer set stops it"
setup
echo '[{"url":"http://127.0.0.1:9701","publicKey":"02ff"}]' >"$(dirname "$SET")/cosigners.json"
code=$(run)
expect '[[ $code != 0 ]] && grep -q "not in the signer set" "$T/out"' "fails, says why"

echo "10. a signer that won't start: nothing rolls back (round 7, W2), everything paused, the bridge stopped"
setup
code=$(FAIL_START=dogevm-signer-2 run)
expect '[[ $code != 0 && $(version) == new && $("$SIGNER_BIN" version) == new ]]' "fails, the new binaries stay"
expect 'paused && all_signers_paused' "bridge and signers paused"
expect 'grep -q "nothing is rolled back" "$T/out"' "says so"
expect 'grep -q "To carry on" "$T/out" && grep -q -- "-fence dogecoin" "$T/out" && grep -q -- "-fence dogecoinvm" "$T/out" && grep -q "signer-log status" "$T/out" && grep -q "resume -signers" "$T/out"' "prints how to carry on: status, start, fence and resume commands (rehearsal, 10 Oct)"

echo "11. a signer that won't stop: nothing is installed or backed up"
setup
code=$(FAIL_STOP=dogevm-signer-2 run)
expect '[[ $code != 0 && $(version) == old ]]' "fails, old binary kept"
expect 'paused && all_signers_paused' "bridge and signers paused"
expect '[[ -z $(ls "$BACKUPS"/*/coordinator 2>/dev/null) ]]' "no backup taken while a signer could sign"

echo "12. partial start: signer 2 runs and signs, signer 1 won't start; nothing rolls back (round 7, W2)"
setup
code=$(LATE_WRITE=dogevm-signer-2 FAIL_START=dogevm-signer-1 run)
expect '[[ $code != 0 ]]' "fails"
expect 'grep -q SIGNED-LATE "$VARLIB/dogevm-signer-2/signing-log.json"' "the log signer 2 wrote is kept"
expect '[[ $(version) == new && $("$SIGNER_BIN" version) == new ]]' "the new binaries stay: the old one must never follow a signature the new one gave"
expect 'paused && all_signers_paused' "bridge and signers paused"

echo "13. a signer whose nodes see another anchor line stops it"
setup
code=$(TIPS_dogevm_signer_2='{"dogecoin":"ff:0","dogecoinvm":"bb:1"}' run)
expect '[[ $code != 0 && $(version) == old ]]' "fails, old binary back"
expect 'grep -q "anchors aren.t the coordinator.s tips" "$T/out"' "says why"

echo "14. contribution lists: the coordinator's goes next to each signer's set; a failure puts theirs back"
setup
c=$(printf '%064d' 1):0
echo "$c" >"$(dirname "$SET")/contributions.txt"
echo "$(printf '%064d' 9):0" >"$VARLIB/dogevm-signer-1/contributions.txt"
code=$(run)
expect '[[ $code == 0 ]]' "succeeds"
expect 'grep -qx "$c" "$VARLIB/dogevm-signer-1/contributions.txt" && grep -qx "$c" "$VARLIB/dogevm-signer-3/contributions.txt"' "every signer reads the coordinator's"
setup
echo "$c" >"$(dirname "$SET")/contributions.txt"
echo "$(printf '%064d' 9):0" >"$VARLIB/dogevm-signer-1/contributions.txt"
code=$(TIPS_dogevm_signer_2='{"dogecoin":"ff:0","dogecoinvm":"bb:1"}' run)
expect '[[ $code != 0 ]] && grep -q "$(printf "%064d" 9):0" "$VARLIB/dogevm-signer-1/contributions.txt"' "signer 1's own list back after a failure"
expect '[[ ! -f $VARLIB/dogevm-signer-2/contributions.txt ]]' "and none where there was none"

echo "15. a contribution list naming transactions, not outputs, is refused before anything changes"
setup
printf '%064d\n' 1 >"$(dirname "$SET")/contributions.txt"
code=$(run)
expect '[[ $code != 0 ]] && grep -q "TXID:VOUT" "$T/out"' "fails, says why"
expect '! paused' "before pausing anything"

echo "16. an anchor a confirmed step behind the tip is fine; one that leads elsewhere stops it (round 4, T7)"
setup
code=$(ANCHORS_dogevm_signer_2='{"dogecoin":"a0:1","dogecoinvm":"bb:1"}' run)
expect '[[ $code == 0 ]]' "behind but leading to the tip: succeeds"
setup
code=$(ANCHORS_dogevm_signer_2='{"dogecoin":"a0:1","dogecoinvm":"bb:1"}' WALKED_dogevm_signer_2='{"dogecoin":"error: the line ended","dogecoinvm":"bb:1"}' run)
expect '[[ $code != 0 && $(version) == old ]]' "leading elsewhere: fails, old binary back"
expect 'grep -q "anchor leads to error" "$T/out"' "says where it leads"

echo "17. unproven logs cut over: fences with the bridge unit's own flags, then RESUME=1 resumes (round 5, U1, U9)"
setup
code=$(NEEDS_FENCE=1 RESUME=1 run)
expect '[[ $code == 0 ]]' "succeeds ($(tail -2 "$T/out" | tr '\n' ' '))"
expect 'grep -q "starting a cutover" "$T/out"' "each unproven log starts a cutover"
expect 'grep -q "^fence dogecoin .*-deposits $STATE/registry/deposits.json" "$EVENTS" && grep -q "^fence dogecoinvm " "$EVENTS"' "a fence on each chain, with the unit's -deposits"
expect '[[ $(line_of "^fence dogecoin ") -lt $(line_of "^resume") ]]' "fences before any resume"
expect '[[ $(line_of "^start dogevm-web-main") -lt $(line_of "^fence dogecoin ") && $(line_of "^fence dogecoin ") -lt $(line_of "^start dogevm-bridge-main") ]]' "web wallet up while fencing; the bridge starts only after (round 6, V7)"
expect '! paused && ! signer_paused 1' "resumed once the cutover is done"

echo "18. a cutover that isn't done never resumes, even with RESUME=1"
setup
code=$(NEEDS_FENCE=1 FENCES_NEEDED=99 RESUME=1 run)
expect '[[ $code != 0 && $(version) == new ]]' "exits non-zero; the upgrade itself stands (no rollback once a fence may be signed)"
expect '! grep -q "^start dogevm-bridge-main" "$EVENTS"' "the bridge stays stopped (round 6, V7)"
expect 'paused && all_signers_paused' "bridge and signers stay paused"
expect 'grep -q "cutover isn.t finished" "$T/out" && grep -q "bridge -signers .* -fence dogecoinvm" "$T/out"' "says so, prints the fence commands"
expect '! grep -q "^resume" "$EVENTS"' "nothing resumed"
expect '[[ $code == 4 ]] && grep -q "runuser -u dogevm-signer-1 -- .* resume -signers" "$T/out" && grep -q "systemctl start dogevm-bridge-main" "$T/out"' "exit 4, with the steps for after it's verified (round 9, Y5)"

echo "19. an old log whose anchors walk to the tip is still cut over (Grok r5 #1)"
setup
code=$(NEEDS_FENCE=1 FENCES_NEEDED=99 run)
expect 'grep -q "dogevm-signer-2: unproven log: starting a cutover" "$T/out"' "signer 2 cut over though its anchors agree"
expect '! grep -q "dogevm-signer-2: anchors lead" "$T/out"' "not just compared"

echo "20. an adopted log is in a cutover from the start"
setup
rm "$VARLIB/dogevm-signer-3/signing-log.json"
code=$(ADOPT_LOGS=dogevm-signer-3 FENCES_NEEDED=99 run)
expect '[[ $code != 0 ]] && grep -q "dogevm-signer-3: in cutover" "$T/out"' "signer 3 in a cutover; unfinished, so non-zero"
expect 'grep -q "dogevm-signer-1: anchors lead" "$T/out"' "proven signers just compared"
expect 'grep -q "^fence dogecoin " "$EVENTS" && paused' "fences sent, bridge paused"

echo "21. a signer in a cutover whose nodes see another tip stops it"
setup
code=$(NEEDS_FENCE=1 TIPS_dogevm_signer_2='{"dogecoin":"ff:0","dogecoinvm":"bb:1"}' run)
expect '[[ $code != 0 && $(version) == old ]] && grep -q "don.t see the coordinator.s tips" "$T/out"' "fails, old binary back, says why"

echo "22. fresh health before resuming: a missing check means no resume (round 6, V7)"
setup
code=$(NEEDS_FENCE=1 RESUME=1 HEALTH='{"checks":[{"name":"pause","ok":false},{"name":"peg","ok":true},{"name":"bridge","ok":true},{"name":"dogecoinvm","ok":true}]}' run)
expect '[[ $code == 0 ]] && grep -q "not resuming: failing checks: dogecoin: missing" "$T/out"' "says the dogecoin check is missing"
expect 'paused && all_signers_paused' "nothing resumed"

echo "24. a signer whose registry or coins differ from the coordinator's stops it before anything is adopted (round 7, W7)"
setup
rm "$VARLIB/dogevm-signer-3/signing-log.json"
code=$(ADOPT_LOGS=dogevm-signer-3 COVERAGE_dogevm_signer_2='{"registry":"r2","registered":1,"coins":{"dogecoin":["aa:0"],"dogecoinvm":["bb:1"]}}' run)
expect '[[ $code != 0 && $(version) == old ]]' "fails, old binary back"
expect 'grep -q "dogevm-signer-2 doesn.t cover what the coordinator covers: registry differs" "$T/out"' "says which and why"
expect '[[ ! -f $VARLIB/dogevm-signer-3/signing-log.json ]]' "nothing adopted"
setup
code=$(COVERAGE_dogevm_signer_1='{"registry":"r1","registered":2,"coins":{"dogecoin":["aa:0","cc:2"],"dogecoinvm":["bb:1"]}}' run)
expect '[[ $code != 0 ]] && grep -q "only here" "$T/out"' "a coin only one signer sees stops it too"

echo "23. once the signers run, nothing rolls back: a bridge that won't start leaves the new binary, everything paused"
setup
code=$(FAIL_START=dogevm-bridge-main RESUME=1 run)
expect '[[ $code != 0 && $(version) == new ]]' "fails, new binary stays"
expect 'paused && all_signers_paused' "bridge and signers paused"

echo "26. a resume that fails part-way pauses every signer again (round 8, X4)"
setup
code=$(FAIL_RESUME=dogevm-signer-2 RESUME=1 run)
expect '[[ $code != 0 ]]' "fails"
expect 'grep -q "^resume .*dogevm-signer-1" "$EVENTS"' "signer 1 had been resumed"
expect 'all_signers_paused && paused' "every signer, and the bridge, paused again"

echo "27. a signer that can't be paused again is stopped (round 8, X4)"
setup
code=$(FAIL_RESUME=dogevm-signer-2 FAIL_PAUSE=dogevm-signer-1 RESUME=1 run)
expect '[[ $code != 0 && ! -f $T/active/dogevm-signer-1 ]]' "signer 1, resumed and unpausable, is stopped"
expect 'grep -q "dogevm-signer-1: stopped (could not be paused" "$T/out"' "says so"

echo "28. not ready to adopt (inventory): stops with exit 3, before any log is adopted (round 8, X5)"
setup
rm "$VARLIB/dogevm-signer-3/signing-log.json"
code=$(INVENTORY_PROBLEM="dogecoin: aa:0 has 3 of 20 confirmations" ADOPT_LOGS="dogevm-signer-3" run)
expect '[[ $code == 3 ]] && grep -q "not ready to adopt: dogecoin: aa:0 has 3 of 20" "$T/out"' "exit 3, says why"
expect '[[ ! -f $VARLIB/dogevm-signer-3/signing-log.json && $(version) == old ]] && paused' "nothing adopted, old binary back, paused"

echo "29. a signer whose cutover recorded something else stops it with exit 2, before any signer starts (round 8, X5)"
setup
code=$(NEEDS_FENCE=1 CUTOVER_LEFT='{"dogecoin":["aa:0","zz:5"],"dogecoinvm":["bb:1"]}' run)
expect '[[ $code == 2 ]] && grep -q "recorded only here" "$T/out"' "exit 2, says what differs"
expect '[[ $(version) == old ]] && paused && all_signers_paused' "old binary back, everything paused"

echo "30. a coverage difference before adopting is exit 2 too (round 8, X5)"
setup
code=$(COVERAGE_dogevm_signer_1='{"registry":"r2","registered":2,"coins":{"dogecoin":["aa:0"],"dogecoinvm":["bb:1"]}}' run)
expect '[[ $code == 2 ]]' "exit 2"

echo "31. a pause that fails part-way before the point of no return: only paused signers start again (round 9, Y3)"
setup
code=$(FAIL_PAUSE_ALWAYS=dogevm-signer-2 run)
expect '[[ $code != 0 && $(version) == old ]]' "fails, old binary back"
expect '[[ ! -f $T/active/dogevm-signer-2 ]] && ! signer_paused 2' "signer 2, unpausable, is left stopped"
expect '[[ -f $T/active/dogevm-signer-1 && -f $T/active/dogevm-signer-3 ]] && signer_paused 1 && signer_paused 3' "signers 1 and 3 run, paused (3 was paused by the cleanup)"
expect 'grep -q "dogevm-signer-2: stopped (could not be paused" "$T/out"' "says so"

echo "32. a signer that can't be paused, stopped or killed is reported as running unpaused, never as safe (round 9, Y3)"
setup
code=$(FAIL_RESUME=dogevm-signer-2 FAIL_PAUSE=dogevm-signer-1 FAIL_STOP_RESUMED=dogevm-signer-1 FAIL_KILL=dogevm-signer-1 RESUME=1 run)
expect '[[ $code != 0 ]]' "fails"
expect 'grep -q "dogevm-signer-1: STILL RUNNING AND NOT PAUSED" "$T/out"' "says signer 1 is exposed"
expect '! grep -q "no signer is running unpaused" "$T/out"' "doesn't claim everything is safe"

echo "33. an inventory that can't run is exit 1, not exit 3 (round 9, Y5)"
setup
code=$(INVENTORY_FAIL="dial tcp 127.0.0.1:22555: connection refused" run)
expect '[[ $code == 1 ]] && grep -q "couldn.t run (not a readiness verdict): dial tcp" "$T/out"' "exit 1, says it couldn't run"
expect '[[ $(version) == old ]] && paused && all_signers_paused' "old binary back, everything paused"

echo "34. a command failing unexpectedly ends the run with exit 1, whatever its own status (round 10, Z5)"
setup
code=$(ANCHORS_TIPS_STATUS=3 run)
expect '[[ $code == 1 ]] && grep -q "a command failed (status 3" "$T/out"' "exit 1, not 3, and says which status"

echo "35. a status that prints more than JSON stops the run, never steers it (rehearsal, 8 Oct)"
setup
code=$(NEEDS_FENCE=1 STATUS_NOISE=1 run)
expect '[[ $code == 1 ]] && grep -q "signer-log status didn.t print JSON" "$T/out"' "exit 1, says the status wasn't JSON"
expect '! grep -q "anchors lead to the coordinator" "$T/out"' "doesn't take the proven-log branch"
expect '[[ $(version) == old ]] && paused && all_signers_paused' "old binary back, everything paused"

echo
echo "$pass passed, $fail failed"
[[ $fail == 0 ]]
