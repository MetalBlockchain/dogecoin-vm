#!/usr/bin/env bash
# Exercises deploy/move-web-state.sh on a fake host: stub systemctl (which
# keeps each unit's state), curl and sleep. Run from the repo root:
#
#   deploy/test/move-web-state-test.sh      # BASH5=$(brew --prefix)/bin/bash on macOS
set -uo pipefail

SCRIPT=$(cd "$(dirname "$0")/.." && pwd)/move-web-state.sh
pass=0 fail=0
ok() { echo "  ok: $*"; pass=$((pass + 1)); }
bad() { echo "  FAIL: $*"; fail=$((fail + 1)); }
expect() { if eval "$1"; then ok "$2"; else bad "$2"; fi; }

# setup [secrets]: a fake host with the live layout (registry in secrets,
# the index in the state directory, or in secrets if asked, finality
# samples beside it).
setup() {
  T=$(mktemp -d)
  export STATE=$T/state UNITDIR=$T/units MOVE_TEST=1 EVENTS=$T/events
  local idx=$STATE/dogeindex
  [[ ${1:-} == secrets ]] && idx=$STATE/secrets/dogeindex
  mkdir -p "$STATE/secrets" "$idx" "$UNITDIR" "$T/bin" "$T/active"
  echo '["aa"]' >"$STATE/secrets/deposits.json"
  echo '{"unsettled":1}' >"$STATE/secrets/dogecoin-unsettled.json"
  echo '{}' >"$(dirname "$idx")/finality.json"
  echo db >"$idx/CURRENT"
  local reg="-deposits $STATE/secrets/deposits.json -confirmations 20"
  printf '[Service]\nExecStart=/x/dogevm bridge -signers s %s -interval 30s\n' "$reg" >"$UNITDIR/dogevm-bridge-main.service"
  printf '[Service]\nExecStart=/x/dogevm serve -signers s %s -doge-index %s -listen 127.0.0.1:8081\n' "$reg" "$idx" >"$UNITDIR/dogevm-web-main.service"
  printf '[Service]\nExecStart=/x/dogevm monitor -signers s %s\n' "$reg" >"$UNITDIR/dogevm-monitor-main.service"
  touch "$T/active/dogevm-bridge-main" "$T/active/dogevm-web-main" "$T/active/dogevm-monitor-main"
  : >"$EVENTS"
  # DOWN_UNIT won't start; STUCK_UNIT won't stop (after STUCK_AFTER stops).
  cat >"$T/bin/systemctl" <<EOF
#!/usr/bin/env bash
cmd=\$1; shift
case \$cmd in
  stop) n=\$(grep -c '^stop-call' "\$EVENTS"); echo stop-call >>"\$EVENTS"
    for u in "\$@"; do
      if [[ \${STUCK_UNIT:-} == "\$u" && \$n -ge \${STUCK_AFTER:-0} ]]; then continue; fi
      rm -f "$T/active/\$u"
    done ;;
  start) for u in "\$@"; do [[ \${DOWN_UNIT:-} == "\$u" ]] || touch "$T/active/\$u"; done ;;
  is-active) [[ -f "$T/active/\${@: -1}" ]]; exit ;;
esac
exit 0
EOF
  cat >"$T/bin/curl" <<'EOF'
#!/usr/bin/env bash
echo "${HEALTH_CODE:-200}"
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' >"$T/bin/sleep"
  chmod +x "$T"/bin/*
  export PATH=$T/bin:$PATH
}
run() { "${BASH5:-bash}" "$SCRIPT" >"$T/out" 2>&1; echo $?; }
W() { echo "$STATE/web"; }

echo "1. the live layout: registry, index and finality samples move; the units follow"
setup
code=$(run)
expect '[[ $code == 0 ]]' "exits 0 ($(tail -2 "$T/out" | tr '\n' ' '))"
expect '[[ -f $(W)/deposits.json && -f $(W)/dogeindex/CURRENT && -f $(W)/finality.json ]]' "files moved"
expect '[[ ! -e $STATE/secrets/deposits.json && ! -e $STATE/dogeindex && ! -e $STATE/finality.json ]]' "nothing left behind"
expect 'grep -q -- "-deposits $(W)/deposits.json -confirmations" "$UNITDIR/dogevm-bridge-main.service"' "bridge unit updated"
expect 'grep -q -- "-doge-index $(W)/dogeindex -listen" "$UNITDIR/dogevm-web-main.service"' "web unit's index updated"
expect 'grep -q -- "-finality $(W)/finality.json" "$UNITDIR/dogevm-web-main.service"' "web unit's finality set"
expect '! grep -q secrets/deposits "$UNITDIR"/*.service' "no unit names the old registry"
expect '[[ -f $(W)/dogecoin-unsettled.json && ! -e $STATE/secrets/dogecoin-unsettled.json ]]' "the unsettled-transaction record moves with the registry (round 6, V2)"
code=$(run)
expect '[[ $code == 0 ]] && grep -q "already done" "$T/out"' "running it again changes nothing"

echo "2. the older layout, with the index in the secrets directory: it moves out too"
setup secrets
code=$(run)
expect '[[ $code == 0 && -f $(W)/dogeindex/CURRENT && ! -e $STATE/secrets/dogeindex ]]' "index moved out of secrets"
expect 'grep -q -- "-doge-index $(W)/dogeindex" "$UNITDIR/dogevm-web-main.service"' "and the unit follows"

echo "3. a web server that won't come back puts everything back"
setup
code=$(DOWN_UNIT=dogevm-web-main run)
expect '[[ $code != 0 ]]' "fails"
expect '[[ -f $STATE/secrets/deposits.json && -f $STATE/dogeindex/CURRENT && -f $STATE/finality.json ]]' "files back"
expect 'grep -q -- "-deposits $STATE/secrets/deposits.json" "$UNITDIR/dogevm-web-main.service" && ! grep -q -- -finality "$UNITDIR/dogevm-web-main.service"' "units back"

echo "4. something already at a destination (a half-done earlier run) is never overwritten"
setup
mkdir -p "$(W)"
echo '["newer"]' >"$(W)/deposits.json"
code=$(run)
expect '[[ $code != 0 ]] && grep -q "already exists; not overwriting" "$T/out"' "fails, says why"
expect 'grep -q newer "$(W)/deposits.json" && grep -q aa "$STATE/secrets/deposits.json"' "both copies kept"
setup
mkdir -p "$(W)"
echo '{}' >"$(W)/finality.json"
code=$(run)
expect '[[ $code != 0 && -f $STATE/secrets/deposits.json && -f $STATE/dogeindex/CURRENT ]]' "a later collision moves back what moved before it"

echo "5. a unit that won't stop: nothing moves"
setup
code=$(STUCK_UNIT=dogevm-web-main run)
expect '[[ $code != 0 && -f $STATE/secrets/deposits.json && ! -e $(W)/deposits.json ]]' "fails, nothing moved"

echo "6. a unit that won't stop on the way back: what moved stays, and it says so"
setup
code=$(DOWN_UNIT=dogevm-web-main STUCK_UNIT=dogevm-bridge-main STUCK_AFTER=1 run)
expect '[[ $code != 0 && -f $(W)/deposits.json ]]' "nothing moved back under a running unit"
expect 'grep -q "nothing is moved back" "$T/out"' "says so"

echo "7. a bridge reading another registry is refused before anything stops"
setup
sed 's# -deposits [^ ]*##' "$UNITDIR/dogevm-bridge-main.service" >"$T/x" && mv "$T/x" "$UNITDIR/dogevm-bridge-main.service"
code=$(run)
expect '[[ $code != 0 ]] && grep -q "isn.t the web server.s" "$T/out"' "fails, says why"
expect '[[ ! -s $EVENTS ]]' "nothing stopped"

echo "8. an unhealthy bridge is left alone"
setup
code=$(HEALTH_CODE=500 run)
expect '[[ $code != 0 && -f $STATE/secrets/deposits.json && ! -s $EVENTS ]]' "fails, nothing stopped or moved"

echo
echo "$pass passed, $fail failed"
[[ $fail == 0 ]]
