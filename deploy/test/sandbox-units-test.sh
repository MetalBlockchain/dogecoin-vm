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
  cat >"$T/bin/curl" <<'EOF'
#!/usr/bin/env bash
code=$(cat "$HEALTH_FILE")
for a in "$@"; do [[ $a == -w ]] && { echo "$code"; exit 0; }; done
[[ $code == 200 ]]
EOF
  printf '#!/usr/bin/env bash\nexit 0\n' >"$T/bin/sleep"
  printf '#!/usr/bin/env bash\nexit 0\n' >"$T/bin/journalctl"
  chmod +x "$T"/bin/*
  export PATH=$T/bin:$PATH
}
run() { "${BASH5:-bash}" "$SCRIPT" "$@" >"$T/out" 2>&1; echo $?; }
web_dropin() { cat "$SYSTEMD_DIR/dogevm-web-main.service.d/10-sandbox.conf"; }

echo "1. apply: the web server may write its own directory, its index and finality, nothing in secrets"
setup
code=$(run)
expect '[[ $code == 0 ]]' "exits 0 ($(tail -1 "$T/out"))"
expect 'web_dropin | grep -qx "ReadWritePaths=$STATE/web -$STATE/web/dogeindex $STATE/web"' "web writes only its own paths"
expect '! web_dropin | grep "^ReadWritePaths" | grep -q secrets' "nothing in secrets"
expect 'grep -q "InaccessiblePaths=.*separate-signers/coordinator" <(web_dropin)' "the coordinator's key hidden from it"

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
expect '[[ $code != 0 ]] && grep -q "answers 500" "$T/out"' "unhealthy after: fails, says so"
setup
echo 503 >"$HEALTH_FILE"
code=$(AFTER=503 run remove)
expect '[[ $code == 0 ]]' "503 before and after (paused): fine"
setup
code=$(AFTER=503 run remove)
expect '[[ $code != 0 ]]' "200 before, 503 after: fails"

echo
echo "$pass passed, $fail failed"
[[ $fail == 0 ]]
