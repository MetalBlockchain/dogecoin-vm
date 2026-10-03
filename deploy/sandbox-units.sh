#!/usr/bin/env bash
# Sandboxes the bridge, web wallet and monitor with systemd drop-ins, as the
# separate signers already are. They run as the same user as the Metal
# node, so without this a bug in the public web server could reach the
# node's database or the bridge's secrets. Run as root on the bridge's host,
# with the bridge healthy:
#
#   deploy/sandbox-units.sh          # apply, restart, check
#   deploy/sandbox-units.sh remove   # take the drop-ins away again
#
# Each service gets a read-only system, no home directories, no new
# privileges, no capabilities, a private /tmp, a syscall filter, only the
# network families it uses, and write access only where it writes:
#   bridge   the secrets directory (pause, lock) and the deposit
#            registry's directory
#   web      the deposit registry's directory (it registers deposit
#            addresses), its coin index and its finality samples; nothing
#            in the secrets directory (the pause file, signer set,
#            cosigners.json and contribution list stay read-only to it)
#   monitor  nothing
# The registry and finality samples must sit in a directory of their own
# first (deploy/move-web-state.sh): a web server that can write the secrets
# directory could remove the pause or rewrite the contribution list.
# The node's own data (database, chain data, plugins) is hidden from all
# three, the coordinator's key from the web and monitor, and the Telegram
# token from the bridge and web. If a service doesn't come back up, or
# /api/health doesn't answer 200, the previous drop-ins (if any) go back.
set -euo pipefail

CHAIN=${CHAIN:-dogevm}
STATE=${STATE:-/var/lib/metal-main}
DROPIN=10-sandbox.conf
UNITS=("$CHAIN-bridge-main" "$CHAIN-web-main" "$CHAIN-monitor-main")
SECRETS=$STATE/secrets
SET=${SET:-$SECRETS/separate-signers/signers.json}
# The coordinator's record of unsettled Dogecoin transactions (round 6, V2)
# sits beside the signer set: the bridge writes it, the web server and the
# monitor may only read it.
UNSETTLED=$(dirname "$SET")/dogecoin-unsettled.json

die() { echo "sandbox-units: $*" >&2; exit 1; }
[[ $(id -u) == 0 || -n ${SANDBOX_TEST:-} ]] || die "run as root"
SYSTEMD_DIR=${SYSTEMD_DIR:-/etc/systemd/system}
path_of() { echo "$SYSTEMD_DIR/$1.service.d/$DROPIN"; }

web_start=$(systemctl show -p ExecStart --value "$CHAIN-web-main")
WEB=$(sed -nE 's/.* -listen ([^ ;]+).*/\1/p' <<<"$web_start")
INDEX=$(sed -nE 's/.* -[a-z]+-index ([^ ;]+).*/\1/p' <<<"$web_start")
REGISTRY=$(sed -nE 's/.* -deposits ([^ ;]+).*/\1/p' <<<"$web_start")
FINALITY=$(sed -nE 's/.* -finality ([^ ;]+).*/\1/p' <<<"$web_start")
[[ -n $WEB ]] || die "can't read the web server's -listen"
[[ -n ${SANDBOX_TEST:-} ]] && systemd-analyze() { :; }
# health_now: "ok" (200 and every expected check passing), "paused" (503
# with every expected check present and the pause the only one failing), or
# "bad CODE: why". The body is read either way: a pause is expected around
# this script, a broken VM or node check behind the same code is not, and a
# response missing a check proves nothing about it.
health_now() {
  local body code
  body=$(curl -s --max-time 30 -w $'\n%{http_code}' "http://$WEB/api/health" || true)
  code=${body##*$'\n'}
  body=${body%$'\n'*}
  if [[ $code == 200 || $code == 503 ]]; then
    python3 - "$code" "$body" <<'PY'
import json, sys
code, body = sys.argv[1], sys.argv[2]
want = ["pause", "peg", "bridge", "dogecoin", "dogecoinvm"]
try:
    checks = {c["name"]: bool(c["ok"]) for c in json.loads(body)["checks"]}
except Exception as e:
    print(f"bad {code}: the health response doesn't parse ({e})")
    sys.exit(0)
missing = [n for n in want if n not in checks]
failing = sorted(n for n, ok in checks.items() if not ok)
if missing:
    print(f"bad {code}: missing checks " + ", ".join(missing))
elif code == "200" and not failing:
    print("ok")
elif code == "503" and failing == ["pause"]:
    print("paused")
else:
    print(f"bad {code}: failing " + ", ".join(failing or ["(none named)"]))
PY
    return
  fi
  echo "bad $code"
}

if [[ ${1:-} == remove ]]; then
  # The bridge must answer afterwards healthy, or paused with nothing else
  # failing, or the removal is a failure.
  before=$(health_now)
  for u in "${UNITS[@]}"; do rm -f "$(path_of "$u")"; done
  systemctl daemon-reload
  systemctl restart "${UNITS[@]}"
  sleep 15
  for u in "${UNITS[@]}"; do
    systemctl is-active --quiet "$u" || die "$u didn't come back up without the sandbox: journalctl -u $u"
  done
  after=$(health_now)
  [[ $after == ok || ( $after == paused && $before == paused ) ]] ||
    die "removed, but /api/health is $after (it was $before before): journalctl -u $CHAIN-web-main"
  echo "sandbox-units: removed; /api/health is $after"
  exit 0
fi

# The web server writes only directories of its own.
[[ -n $REGISTRY ]] || die "the web server has no -deposits"
[[ -n $INDEX && -n $FINALITY ]] || die "the web server needs -doge-index and -finality: run deploy/move-web-state.sh first"
for dir in "$(dirname "$REGISTRY")" "$(dirname "$FINALITY")"; do
  case $dir in
    "$SECRETS" | "$SECRETS"/* | "$STATE" | / | "$(dirname "$STATE")")
      die "the web server would write $dir, which holds more than its own files: run deploy/move-web-state.sh first" ;;
  esac
done
case $INDEX in "$SECRETS" | "$SECRETS"/*) die "the coin index $INDEX is inside the secrets directory; move it out first" ;; esac
for dir in "$(dirname "$REGISTRY")" "$(dirname "$FINALITY")" "$INDEX"; do
  case $UNSETTLED in "$dir"/*) die "the web server would write $dir, which holds the coordinator's $UNSETTLED" ;; esac
done
before=$(health_now)
[[ $before == ok || $before == paused ]] || die "the bridge isn't healthy now ($before; http://$WEB/api/health); fix that first"

BACKUP=$(mktemp -d)
for u in "${UNITS[@]}"; do
  [[ -f $(path_of "$u") ]] && cp -p "$(path_of "$u")" "$BACKUP/$u"
done
ok=0
cleanup() {
  [[ $ok == 1 ]] && return
  echo "sandbox-units: putting the previous drop-ins back" >&2
  for u in "${UNITS[@]}"; do
    if [[ -f $BACKUP/$u ]]; then cp -p "$BACKUP/$u" "$(path_of "$u")"; else rm -f "$(path_of "$u")"; fi
  done
  systemctl daemon-reload
  systemctl restart "${UNITS[@]}" || true
}
trap cleanup EXIT

common() {
  cat <<EOF
# Written by deploy/sandbox-units.sh.
[Service]
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictNamespaces=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
CapabilityBoundingSet=
AmbientCapabilities=
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
UMask=0077
InaccessiblePaths=-$STATE/node -$STATE/chaindata -$STATE/chainlogs -$STATE/plugins
EOF
}
write() {
  mkdir -p "$(dirname "$(path_of "$1")")"
  { common; printf '%s\n' "$2"; } >"$(path_of "$1")"
}
write "$CHAIN-bridge-main" "ReadWritePaths=$SECRETS $(dirname "$REGISTRY")
InaccessiblePaths=-$SECRETS/telegram-token"
# The web server renames files in the registry's directory and beside its
# finality samples, and writes its coin index; everything else, the
# secrets directory included, stays read-only to it.
write "$CHAIN-web-main" "ReadWritePaths=$(dirname "$REGISTRY") -$INDEX $(dirname "$FINALITY")
ReadOnlyPaths=-$STATE/chain-configs -$STATE/genesis.json -$STATE/chain.json -$UNSETTLED
InaccessiblePaths=-$SECRETS/separate-signers/coordinator -$SECRETS/telegram-token -$SECRETS/rotation-1"
write "$CHAIN-monitor-main" "ReadOnlyPaths=$SECRETS -$UNSETTLED
InaccessiblePaths=-$SECRETS/separate-signers/coordinator"

systemctl daemon-reload
systemctl restart "${UNITS[@]}"
sleep 15
for u in "${UNITS[@]}"; do
  systemctl is-active --quiet "$u" || { journalctl -u "$u" -n 20 --no-pager >&2 || true; die "$u didn't stay up under the sandbox"; }
done
after=$(health_now)
[[ $after == ok || ( $after == paused && $before == paused ) ]] || die "under the sandbox /api/health is $after (it was $before before)"
ok=1
trap - EXIT
rm -rf "$BACKUP"
for u in "${UNITS[@]}"; do systemd-analyze security "$u" 2>/dev/null | tail -1; done
echo "sandbox-units: done. Now register a new deposit address on the site, and watch the next bridge pass:"
echo "  journalctl -fu $CHAIN-bridge-main"
