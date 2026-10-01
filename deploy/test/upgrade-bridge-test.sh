#!/usr/bin/env bash
# Exercises deploy/upgrade-bridge.sh on a fake host: stub systemctl,
# runuser, journalctl, curl, git and go, and a stub bridge binary that
# pauses, resumes and checks signing logs. Run from the repo root:
#
#   deploy/test/upgrade-bridge-test.sh
set -uo pipefail

SCRIPT=$(cd "$(dirname "$0")/.." && pwd)/upgrade-bridge.sh
pass=0 fail=0
ok() { echo "  ok: $*"; pass=$((pass + 1)); }
bad() { echo "  FAIL: $*"; fail=$((fail + 1)); }
expect() { if eval "$1"; then ok "$2"; else bad "$2"; fi; }

# A stub bridge binary: VERSION is printed by "version"; pause/resume write
# and remove paused.json next to -signers; signer-log checks/creates -log.
stub_bridge() { # path version
  cat >"$1" <<EOF
#!/usr/bin/env bash
cmd=\$1; shift
arg() { local want=\$1; shift; while [[ \$# -gt 0 ]]; do [[ \$1 == \$want ]] && { echo "\$2"; return; }; shift; done; }
case \$cmd in
  version) echo "$2" ;;
  pause) set=\$(arg -signers "\$@"); echo '{"reason":"x"}' >"\$(dirname "\$set")/paused.json" ;;
  resume) set=\$(arg -signers "\$@"); rm -f "\$(dirname "\$set")/paused.json" ;;
  signer-log)
    step=\$1; shift; log=\$(arg -log "\$@")
    case \$step in
      init|adopt) [[ -f \$log ]] && { echo "exists" >&2; exit 1; }; echo '{"actions":{}}' >"\$log" ;;
      check) [[ -f \$log ]] || { echo "no log" >&2; exit 1; }; grep -q QUARANTINE "\$log" && { echo "quarantined" >&2; exit 1; }; true ;;
    esac ;;
esac
EOF
  chmod +x "$1"
}

setup() { # makes a fresh fake host in $T
  T=$(mktemp -d)
  export HOME_DIR=$T/opt STATE=$T/state VARLIB=$T/varlib BACKUPS=$T/backups SIGNER_BIN=$T/signerbin/dogevm UPGRADE_TEST=1
  export SET=$STATE/secrets/separate-signers/signers.json BRANCH=main
  mkdir -p "$HOME_DIR/bin" "$HOME_DIR/src" "$(dirname "$SET")" "$BACKUPS" "$(dirname "$SIGNER_BIN")" "$T/bin" "$T/units"
  stub_bridge "$HOME_DIR/bin/dogevm" old
  stub_bridge "$SIGNER_BIN" old
  echo '{"required":2,"publicKeys":["02aa","02bb","02cc"]}' >"$SET"
  echo '[{"url":"http://127.0.0.1:9701"},{"url":"http://127.0.0.1:9702"},{"url":"http://127.0.0.1:9703"}]' >"$(dirname "$SET")/cosigners.json"
  local n key
  for n in 1 2 3; do
    key=$(sed -n "${n}p" <<<$'02aa\n02bb\n02cc')
    mkdir -p "$VARLIB/dogevm-signer-$n"
    echo "BITCOIN_NETWORK=mainnet" >"$VARLIB/dogevm-signer-$n/signer.env"
    echo "{\"publicKey\":\"$key\"}" >"$VARLIB/dogevm-signer-$n/card.json"
    echo '{"actions":{}}' >"$VARLIB/dogevm-signer-$n/signing-log.json"
    echo "/x/dogevm signer -signers $SET -key-file $VARLIB/dogevm-signer-$n/signer.key -listen 127.0.0.1:970$n -log $VARLIB/dogevm-signer-$n/signing-log.json -deposits $VARLIB/dogevm-signer-$n/deposits.json -max-daily 1" >"$T/units/dogevm-signer-$n"
  done
  echo "/x/dogevm serve -signers $SET -listen 127.0.0.1:8081" >"$T/units/dogevm-web-main"
  echo "/x/dogevm bridge -signers $SET" >"$T/units/dogevm-bridge-main"
  echo "/x/dogevm monitor -signers $SET" >"$T/units/dogevm-monitor-main"
  : >"$T/events"
  cat >"$T/bin/systemctl" <<EOF
#!/usr/bin/env bash
case \$1 in
  list-unit-files) for n in 1 2 3; do echo "dogevm-signer-\$n.service enabled enabled"; done ;;
  show) unit=\${@: -1}; cat "$T/units/\${unit%.service}" ;;
  stop|start) shift; for u in "\$@"; do echo "\$1 \$u" | sed "s/^/\$1 /" >/dev/null; echo "\${0##*/} \$u" >/dev/null; done
     for u in "\$@"; do echo "\$u" >>"$T/events.\${FUNCNAME:-x}"; done
     [[ \$1 == stop ]] && true ;;
  is-active) unit=\${@: -1}; [[ \${FAIL_UNIT:-} == "\$unit" ]] && exit 3; exit 0 ;;
esac
exit 0
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
if [[ -n ${HEALTH:-} ]]; then echo "$HEALTH"; else echo '{"checks":[{"name":"peg","ok":true}]}'; fi
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
run() { "${BASH5:-bash}" "$SCRIPT" abc123 >"$T/out" 2>&1; echo $?; }
paused() { [[ -f $(dirname "$SET")/paused.json ]]; }
version() { "$HOME_DIR/bin/dogevm" version; }

echo "1. success: new binary, logs checked, keys filled in, still paused"
setup
code=$(run)
expect '[[ $code == 0 ]]' "exits 0 ($(tail -3 "$T/out" | tr '\n' ' '))"
expect '[[ $(version) == new && $("$SIGNER_BIN" version) == new ]]' "both binaries upgraded"
expect 'paused' "left paused"
expect 'grep -q 02bb "$(dirname "$SET")/cosigners.json"' "public keys filled in"

echo "2. a missing signing log stops it and everything goes back"
setup
rm "$VARLIB/dogevm-signer-3/signing-log.json"
code=$(run)
expect '[[ $code != 0 ]]' "fails"
expect 'grep -q "has no signing log" "$T/out"' "says why"
expect '[[ $(version) == old && $("$SIGNER_BIN" version) == old ]]' "old binaries back"
expect 'paused' "still paused"
expect '[[ ! -f $VARLIB/dogevm-signer-3/signing-log.json ]]' "no log invented"

echo "3. INIT_LOGS names a signer that never signed"
setup
rm "$VARLIB/dogevm-signer-3/signing-log.json"
code=$(INIT_LOGS=dogevm-signer-3 run)
expect '[[ $code == 0 ]]' "succeeds"
expect '[[ -f $VARLIB/dogevm-signer-3/signing-log.json ]]' "an empty log made for it"

echo "4. an unhealthy peg after the upgrade rolls back, including a log it made"
setup
rm "$VARLIB/dogevm-signer-3/signing-log.json"
code=$(INIT_LOGS=dogevm-signer-3 HEALTH='{"checks":[{"name":"peg","ok":false}]}' run)
expect '[[ $code != 0 ]]' "fails"
expect '[[ $(version) == old ]]' "old binary back"
expect '[[ ! -f $VARLIB/dogevm-signer-3/signing-log.json ]]' "the log it made is removed"
expect '! grep -q publicKey "$(dirname "$SET")/cosigners.json"' "cosigners.json restored"
expect 'paused' "still paused"

echo "5. a pause already in place stays, even with RESUME=1"
setup
echo '{}' >"$(dirname "$SET")/paused.json"
code=$(RESUME=1 run)
expect '[[ $code == 0 ]]' "succeeds"
expect 'paused' "still paused"

echo "6. RESUME=1 resumes a bridge it paused itself"
setup
code=$(RESUME=1 run)
expect '[[ $code == 0 ]] && ! paused' "resumed"

echo "7. a quarantined signer log stops it"
setup
echo QUARANTINE >>"$VARLIB/dogevm-signer-2/signing-log.json"
code=$(run)
expect '[[ $code != 0 && $(version) == old ]] && paused' "fails, old binary back, paused"

echo "8. a cosigners.json key outside the signer set stops it"
setup
echo '[{"url":"http://127.0.0.1:9701","publicKey":"02ff"}]' >"$(dirname "$SET")/cosigners.json"
code=$(run)
expect '[[ $code != 0 ]] && grep -q "not in the signer set" "$T/out"' "fails, says why"

echo "9. a signer that won't start rolls back"
setup
code=$(FAIL_UNIT=dogevm-signer-2 run)
expect '[[ $code != 0 && $(version) == old ]] && paused' "fails, old binary back, paused"

echo
echo "$pass passed, $fail failed"
[[ $fail == 0 ]]
