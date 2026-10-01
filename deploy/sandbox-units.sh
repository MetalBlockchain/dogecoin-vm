#!/usr/bin/env bash
# Sandboxes the bridge, web wallet and monitor with systemd drop-ins, as the
# separate signers already are. They run as the same user as the Metal
# node, so without this a bug in the public web server could reach the
# node's database or the bridge's secrets. Run as root on the bridge's host:
#
#   deploy/sandbox-units.sh          # apply, restart, check
#   deploy/sandbox-units.sh remove   # take the drop-ins away again
#
# Each service gets a read-only system, no home directories, no new
# privileges, no capabilities, a private /tmp, only the network families it
# uses, and write access only where it writes:
#   bridge   the secrets directory (pause, lock, registry)
#   web      the Bitcoin index and the finality samples
#   monitor  nothing
# The node's own data (database, chain data, plugins) is hidden from all
# three. If a service doesn't come back up, or /api/health doesn't answer,
# the drop-ins are removed and the services restarted as they were.
set -euo pipefail

CHAIN=${CHAIN:-dogevm}
STATE=${STATE:-/var/lib/metal-main}
DROPIN=10-sandbox.conf
UNITS=("$CHAIN-bridge-main" "$CHAIN-web-main" "$CHAIN-monitor-main")

die() { echo "sandbox-units: $*" >&2; exit 1; }
[[ $(id -u) == 0 ]] || die "run as root"

remove() {
  for u in "${UNITS[@]}"; do rm -f "/etc/systemd/system/$u.service.d/$DROPIN"; done
  systemctl daemon-reload
  systemctl restart "${UNITS[@]}"
}

if [[ ${1:-} == remove ]]; then
  remove
  echo "sandbox-units: removed"
  exit 0
fi

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
  local unit=$1 extra=$2
  mkdir -p "/etc/systemd/system/$unit.service.d"
  { common; printf '%s\n' "$extra"; } >"/etc/systemd/system/$unit.service.d/$DROPIN"
}
write "$CHAIN-bridge-main" "ReadWritePaths=$STATE/secrets"
write "$CHAIN-web-main" "ReadWritePaths=-$STATE/btcindex $STATE
ReadOnlyPaths=$STATE/secrets"
write "$CHAIN-monitor-main" "ReadOnlyPaths=$STATE/secrets"

systemctl daemon-reload
systemctl restart "${UNITS[@]}"
sleep 10
for u in "${UNITS[@]}"; do
  if ! systemctl is-active --quiet "$u"; then
    echo "sandbox-units: $u didn't stay up under the sandbox; removing it" >&2
    journalctl -u "$u" -n 20 --no-pager >&2 || true
    remove
    exit 1
  fi
done
if ! curl -sf -o /dev/null --max-time 20 http://127.0.0.1:8081/api/health; then
  # 503 is fine (a check failing); no answer at all is not.
  if ! curl -s -o /dev/null --max-time 20 -w '%{http_code}' http://127.0.0.1:8081/api/health | grep -q '^[2-5]'; then
    echo "sandbox-units: the web server doesn't answer under the sandbox; removing it" >&2
    remove
    exit 1
  fi
fi
for u in "${UNITS[@]}"; do systemd-analyze security "$u" 2>/dev/null | tail -1; done
echo "sandbox-units: done. Watch the next bridge pass in journalctl -u $CHAIN-bridge-main."
