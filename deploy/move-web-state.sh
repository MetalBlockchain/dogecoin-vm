#!/usr/bin/env bash
# Moves what the public web server writes out of the secrets directory and
# out of the state directory's top level, into a directory of its own, so
# deploy/sandbox-units.sh can let the web server write there and nowhere
# else (not the pause file, the signer set, cosigners.json or the
# contribution list beside them). Run as root on the bridge's host, once,
# before sandbox-units.sh:
#
#   deploy/move-web-state.sh
#
# It stops the bridge, web wallet and monitor; moves the deposit registry
# ($SECRETS/deposits.json) and the finality samples (finality.json beside
# the coin index) into $STATE/web; points the units' -deposits (and the web
# server's -finality) there; and starts them again, checking /api/health.
# On a failure the files and units go back.
set -euo pipefail

CHAIN=${CHAIN:-dogevm}
STATE=${STATE:-/var/lib/metal-main}
SECRETS=$STATE/secrets
WEBSTATE=${WEBSTATE:-$STATE/web}
UNITS=("$CHAIN-bridge-main" "$CHAIN-web-main" "$CHAIN-monitor-main")
UNITDIR=${UNITDIR:-/etc/systemd/system}

die() { echo "move-web-state: $*" >&2; exit 1; }
[[ $(id -u) == 0 || -n ${MOVE_TEST:-} ]] || die "run as root"
start_of() { systemctl show -p ExecStart --value "$1"; }
flag_of() { sed -nE "s/.* $2 ([^ ;]+).*/\1/p" <<<"$(start_of "$1")"; }

OLD_REG=$(flag_of "$CHAIN-web-main" -deposits)
[[ -n $OLD_REG ]] || die "the web server has no -deposits"
for u in "$CHAIN-bridge-main" "$CHAIN-monitor-main"; do
  reg=$(flag_of "$u" -deposits)
  [[ -z $reg || $reg == "$OLD_REG" ]] || die "$u's -deposits ($reg) isn't the web server's ($OLD_REG)"
done
INDEX=$(flag_of "$CHAIN-web-main" -doge-index)
OLD_FIN=$(flag_of "$CHAIN-web-main" -finality)
[[ -n $OLD_FIN || -z $INDEX ]] || OLD_FIN=$(dirname "$INDEX")/finality.json
NEW_REG=$WEBSTATE/deposits.json
NEW_FIN=$WEBSTATE/finality.json
if [[ $OLD_REG == "$NEW_REG" && ( -z $OLD_FIN || $OLD_FIN == "$NEW_FIN" ) ]]; then
  echo "move-web-state: already in $WEBSTATE"
  exit 0
fi
WEB=$(flag_of "$CHAIN-web-main" -listen)
health() { curl -s -o /dev/null --max-time 30 -w '%{http_code}' "http://$WEB/api/health" || true; }
BEFORE=$(health)
[[ $BEFORE == 200 || $BEFORE == 503 ]] || die "the web server answers $BEFORE now; fix that first"

BACKUP=$(mktemp -d)
for u in "${UNITS[@]}"; do cp -p "$UNITDIR/$u.service" "$BACKUP/"; done
ok=0
cleanup() {
  [[ $ok == 1 ]] && return
  echo "move-web-state: failed; putting the files and units back" >&2
  systemctl stop "${UNITS[@]}" || true
  [[ -f $NEW_REG && ! -f $OLD_REG ]] && mv "$NEW_REG" "$OLD_REG"
  [[ -n $OLD_FIN && -f $NEW_FIN && ! -f $OLD_FIN ]] && mv "$NEW_FIN" "$OLD_FIN"
  for u in "${UNITS[@]}"; do cp -p "$BACKUP/$u.service" "$UNITDIR/"; done
  systemctl daemon-reload
  systemctl start "${UNITS[@]}" || true
}
trap cleanup EXIT

systemctl stop "${UNITS[@]}"
install -d -m 700 "$WEBSTATE"
[[ -n ${MOVE_TEST:-} ]] || chown "$CHAIN:$CHAIN" "$WEBSTATE"
[[ -f $OLD_REG ]] && mv "$OLD_REG" "$NEW_REG"
[[ -n $OLD_FIN && -f $OLD_FIN ]] && mv "$OLD_FIN" "$NEW_FIN"
for u in "${UNITS[@]}"; do
  sed -i -e "s# -deposits $OLD_REG# -deposits $NEW_REG#" "$UNITDIR/$u.service"
done
if grep -q -- " -finality " "$UNITDIR/$CHAIN-web-main.service"; then
  sed -i -e "s# -finality [^ ]*# -finality $NEW_FIN#" "$UNITDIR/$CHAIN-web-main.service"
else
  sed -i -e "/^ExecStart=/ s#\$# -finality $NEW_FIN#" "$UNITDIR/$CHAIN-web-main.service"
fi
systemctl daemon-reload
systemctl start "${UNITS[@]}"
sleep 15
for u in "${UNITS[@]}"; do
  systemctl is-active --quiet "$u" || die "$u didn't start: journalctl -u $u"
done
code=$(health)
[[ $code == "$BEFORE" ]] || die "the web server answered $BEFORE before the move and $code after"
ok=1
trap - EXIT
rm -rf "$BACKUP"
echo "move-web-state: the registry and finality samples are in $WEBSTATE; now deploy/sandbox-units.sh"
