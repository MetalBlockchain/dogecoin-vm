#!/usr/bin/env bash
# Exercises deploy/sandbox-units.sh on a fake host: stub systemctl, curl,
# journalctl and sleep, with the drop-ins written under a temporary
# directory. It checks what the drop-ins allow, not systemd itself. Run
# from the repo root:
#
#   deploy/test/sandbox-units-test.sh      # BASH5=$(brew --prefix)/bin/bash on macOS
set -uo pipefail

SCRIPT=$(cd "$(dirname "$0")/.." && pwd)/sandbox-units.sh
pass=0 fail=0
ok() { echo "  ok: $*"; pass=$((pass + 1)); }
bad() { echo "  FAIL: $*"; fail=$((fail + 1)); }
expect() { if eval "$1"; then ok "$2"; else bad "$2"; fi; }

# setup [secrets]: the web server's state in $STATE/web (after
# move-web-state.sh), or its registry still in secrets.
setup() {
  T=$(mktemp -d)
  export STATE=$T/state SYSTEMD_DIR=$T/systemd SANDBOX_TEST=1 HEALTH_FILE=$T/health
  mkdir -p "$STATE/web/dogeindex" "$STATE/secrets" "$SYSTEMD_DIR" "$T/bin"
  local reg=$STATE/web/deposits.json
  [[ ${1:-} == secrets ]] && reg=$STATE/secrets/deposits.json
  echo "/x/dogevm serve -signers s -deposits $reg -doge-index $STATE/web/dogeindex -finality $STATE/web/finality.json -listen 127.0.0.1:8081" >"$T/webstart"
  echo 200 >"$HEALTH_FILE"
  cat >"$T/bin/systemctl" <<EOF
#!/usr/bin/env bash
case \$1 in
  show) cat "$T/webstart" ;;
  restart) [[ -n \${AFTER:-} ]] && echo "\$AFTER" >"\$HEALTH_FILE" ;;
esac
exit 0
EOF
  # HEALTH_FILE holds the status code, or "503-pause" (paused, nothing
  # else failing) or "503-vm" (paused, and the VM check failing too).
  cat >"$T/bin/curl" <<'EOF'
#!/usr/bin/env bash
h=$(cat "$HEALTH_FILE")
code=${h%%-*}
case $h in
  503-pause) body='{"checks":[{"name":"pause","ok":false},{"name":"peg","ok":true},{"name":"bridge","ok":true},{"name":"dogecoin","ok":true},{"name":"dogecoinvm","ok":true}]}' ;;
  503-vm) body='{"checks":[{"name":"pause","ok":false},{"name":"peg","ok":true},{"name":"bridge","ok":true},{"name":"dogecoin","ok":true},{"name":"dogecoinvm","ok":false}]}' ;;
  503-held) body='{"checks":[{"name":"pause","ok":false},{"name":"peg","ok":true},{"name":"bridge","ok":false,"detail":"deposit aa:0 (6.00000000 DOGE) is held"},{"name":"dogecoin","ok":true},{"name":"dogecoinvm","ok":true},{"name":"validator","ok":false,"detail":"0.97 METAL"}]}' ;;
  503-heldx) body='{"checks":[{"name":"pause","ok":false},{"name":"peg","ok":true},{"name":"bridge","ok":false,"detail":"permission denied writing the registry"},{"name":"dogecoin","ok":true},{"name":"dogecoinvm","ok":true},{"name":"validator","ok":false,"detail":"0.96 METAL"}]}' ;;
  503-short) body='{"checks":[{"name":"pause","ok":false},{"name":"peg","ok":true}]}' ;;
  503) body='not json' ;;
  200-short) body='{"checks":[{"name":"pause","ok":true}]}' ;;
  *) body='{"checks":[{"name":"pause","ok":true},{"name":"peg","ok":true},{"name":"bridge","ok":true},{"name":"dogecoin","ok":true},{"name":"dogecoinvm","ok":true}]}' ;;
esac
for a in "$@"; do [[ $a == -w ]] && { printf '%s\n%s' "$body" "$code"; exit 0; }; done
[[ $code == 200 ]]
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' >"$T/bin/sleep"
  printf '#!/usr/bin/env bash\nexit 0\n' >"$T/bin/journalctl"
  chmod +x "$T"/bin/*
  export PATH=$T/bin:$PATH
}
run() { "${BASH5:-bash}" "$SCRIPT" "$@" >"$T/out" 2>&1; echo $?; }
web_dropin() { cat "$SYSTEMD_DIR/dogevm-web-main.service.d/10-sandbox.conf"; }
# web_can_write PATH: PATH is, or is under, one of the web unit's
# ReadWritePaths.
web_can_write() {
  local line d
  line=$(web_dropin | grep '^ReadWritePaths=') || return 1
  for d in ${line#ReadWritePaths=}; do
    d=${d#-}
    [[ $1 == "$d" || $1 == "$d"/* ]] && return 0
  done
  return 1
}

echo "1. apply: the web server may write its own directory, its index and finality, nothing in secrets"
setup
code=$(run)
expect '[[ $code == 0 ]]' "exits 0 ($(tail -1 "$T/out"))"
expect 'web_dropin | grep -qx "ReadWritePaths=$STATE/web -$STATE/web/dogeindex $STATE/web"' "web writes only its own paths"
expect '! web_dropin | grep "^ReadWritePaths" | grep -q secrets' "nothing in secrets"
expect 'grep -q "InaccessiblePaths=.*separate-signers/coordinator" <(web_dropin)' "the coordinator's key hidden from it"
U=$STATE/secrets/separate-signers/dogecoin-unsettled.json
expect '! web_can_write "$U"' "the web server can't write the unsettled-transaction record (round 6, V2)"
expect 'web_dropin | grep -q "^ReadOnlyPaths=.*-$U" && grep -q "^ReadOnlyPaths=.*-$U" "$SYSTEMD_DIR/dogevm-monitor-main.service.d/10-sandbox.conf"' "web and monitor: read-only"
expect 'grep "^ReadWritePaths" "$SYSTEMD_DIR/dogevm-bridge-main.service.d/10-sandbox.conf" | grep -q "$STATE/secrets"' "the bridge can write it"

echo "2. apply refuses while the registry is still in secrets"
setup secrets
code=$(run)
expect '[[ $code != 0 ]] && grep -q "move-web-state.sh first" "$T/out"' "fails, says what to run"
expect '[[ ! -e $SYSTEMD_DIR/dogevm-web-main.service.d ]]' "no drop-in written"

echo "3. remove: a bridge that answers 200 afterwards is fine; one that doesn't fails the removal (round 4, T10)"
setup
run >/dev/null
code=$(run remove)
expect '[[ $code == 0 && ! -e $SYSTEMD_DIR/dogevm-web-main.service.d/10-sandbox.conf ]]' "removed"
setup
run >/dev/null
code=$(AFTER=500 run remove)
expect '[[ $code != 0 ]] && grep -q "is bad 500" "$T/out"' "unhealthy after: fails, says so"
setup
echo 503-pause >"$HEALTH_FILE"
code=$(AFTER=503-pause run remove)
expect '[[ $code == 0 ]]' "paused before and after, nothing else failing: fine"
setup
code=$(AFTER=503-pause run remove)
expect '[[ $code != 0 ]]' "200 before, paused after: fails"

echo "4. a 503 counts as paused only when the pause is the only failing check (round 5, U10)"
setup
echo 503-pause >"$HEALTH_FILE"
code=$(AFTER=503-vm run remove)
expect '[[ $code != 0 ]] && grep -q "failing dogecoinvm, pause" "$T/out"' "paused before; paused and the VM failing after: fails, names it"
setup
echo 503 >"$HEALTH_FILE"
code=$(AFTER=503 run remove)
expect '[[ $code != 0 ]] && grep -q "doesn.t parse" "$T/out"' "a 503 whose body doesn't parse: fails"
setup
echo 503-pause >"$HEALTH_FILE"
code=$(AFTER=503-pause run)
expect '[[ $code == 0 ]]' "apply while paused (only the pause failing): fine"
setup
echo 503-vm >"$HEALTH_FILE"
code=$(run)
expect '[[ $code != 0 ]] && [[ ! -e $SYSTEMD_DIR/dogevm-web-main.service.d ]]' "apply with the VM failing behind the pause: refuses, writes nothing"
setup
code=$(AFTER=503-vm run)
expect '[[ $code != 0 ]] && [[ ! -e $SYSTEMD_DIR/dogevm-web-main.service.d/10-sandbox.conf ]]' "apply that breaks the VM check: fails and puts things back"

echo "4a. known failures named in ALLOW_FAILING must fail the same way after (rehearsal, 8 Oct)"
setup
echo 503-held >"$HEALTH_FILE"
code=$(run)
expect '[[ $code != 0 ]] && [[ ! -e $SYSTEMD_DIR/dogevm-web-main.service.d ]]' "a held deposit, not named: refuses, writes nothing"
setup
echo 503-held >"$HEALTH_FILE"
code=$(ALLOW_FAILING=bridge,validator AFTER=503-held run)
expect '[[ $code == 0 ]]' "named, and failing the same way after: fine"
setup
echo 503-held >"$HEALTH_FILE"
code=$(ALLOW_FAILING=bridge,validator AFTER=503-heldx run)
expect '[[ $code != 0 ]] && [[ ! -e $SYSTEMD_DIR/dogevm-web-main.service.d/10-sandbox.conf ]]' "named, but the bridge fails differently after: fails and puts things back"
setup
echo 503-held >"$HEALTH_FILE"
code=$(ALLOW_FAILING=bridge,validator,dogecoinvm run)
expect '[[ $code != 0 ]] && grep -q "can.t name" "$T/out"' "can't allow a core check"

echo "4b. a web directory holding the unsettled-transaction record is refused (round 6, V2)"
setup
sed -i.bak "s#-finality $STATE/web/finality.json#-finality $STATE/secrets/separate-signers/finality.json#" "$T/webstart"
code=$(run)
expect '[[ $code != 0 ]] && [[ ! -e $SYSTEMD_DIR/dogevm-web-main.service.d ]]' "refuses, writes nothing"

echo "5. every expected check must be present, for a 200 and a 503 (round 6, V8)"
setup
echo 503-pause >"$HEALTH_FILE"
code=$(AFTER=503-short run remove)
expect '[[ $code != 0 ]] && grep -q "missing checks bridge, dogecoin, dogecoinvm" "$T/out"' "paused after, but the node checks are missing: fails, names them"
setup
code=$(AFTER=200-short run remove)
expect '[[ $code != 0 ]] && grep -q "missing checks" "$T/out"' "a 200 with only the pause check: fails"

echo
echo "$pass passed, $fail failed"
[[ $fail == 0 ]]
