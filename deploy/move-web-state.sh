#!/usr/bin/env bash
# Gives the files the public web server writes a directory of its own, so
# its sandbox (deploy/sandbox-units.sh) can let it write there and nowhere
# else (not the pause file, the signer set, cosigners.json or the
# contribution list in the secrets directory, nor anything else in the
# state directory). Run as root on the bridge's host, once, before
# sandbox-units.sh:
#
#   deploy/move-web-state.sh
#
#   registry  the web server's -deposits            ->  $WEBSTATE/deposits.json
#   index     its -doge-index, if in secrets or
#             directly in the state directory        ->  $WEBSTATE/<same name>
#   finality  its -finality, or finality.json beside
#             the index                              ->  $WEBSTATE/finality.json
#
# It stops the bridge, web wallet and monitor and checks they are down (the
# signers keep running and keep their own registries); moves the files,
# refusing to overwrite anything already at a destination; points the
# units' -deposits, -doge-index and the web server's -finality at the new
# places; starts them again and checks /api/health answers as it did
# before. On any failure it stops them again, checks they are down, moves
# back exactly what it moved (newest first) and puts the units back. If a
# unit won't stop, it moves nothing (back), and says so. Running it again
# changes nothing.
set -euo pipefail

CHAIN=${CHAIN:-dogevm}
STATE=${STATE:-/var/lib/metal-main}
SECRETS=$STATE/secrets
WEBSTATE=${WEBSTATE:-$STATE/web}
UNITS=("$CHAIN-bridge-main" "$CHAIN-web-main" "$CHAIN-monitor-main")
UNITDIR=${UNITDIR:-/etc/systemd/system}

die() { echo "move-web-state: $*" >&2; exit 1; }
[[ $(id -u) == 0 || -n ${MOVE_TEST:-} ]] || die "run as root"
unit_file() { echo "$UNITDIR/$1.service"; }
flag_of() { sed -nE "s/^ExecStart=.* $2 ([^ ;]+).*/\1/p" "$(unit_file "$1")"; }

OLD_REG=$(flag_of "$CHAIN-web-main" -deposits)
[[ -n $OLD_REG ]] || die "the web server has no -deposits"
for u in "$CHAIN-bridge-main" "$CHAIN-monitor-main"; do
  reg=$(flag_of "$u" -deposits)
  # Without -deposits a unit reads the registry next to its -signers,
  # another file: it must name the web server's.
  [[ $reg == "$OLD_REG" ]] || die "$u's -deposits (${reg:-none: the default next to -signers}) isn't the web server's ($OLD_REG); fix the units first"
done
OLD_IDX=$(flag_of "$CHAIN-web-main" -doge-index)
OLD_FIN=$(flag_of "$CHAIN-web-main" -finality)
[[ -n $OLD_FIN || -z $OLD_IDX ]] || OLD_FIN=$(dirname "$OLD_IDX")/finality.json
NEW_REG=$WEBSTATE/deposits.json
NEW_FIN=$WEBSTATE/finality.json
NEW_IDX=$OLD_IDX
if [[ -n $OLD_IDX ]]; then
  case $(dirname "$OLD_IDX") in
    "$SECRETS" | "$SECRETS"/* | "$STATE") NEW_IDX=$WEBSTATE/$(basename "$OLD_IDX") ;;
  esac
fi
if [[ $OLD_REG == "$NEW_REG" && ( -z $OLD_FIN || $OLD_FIN == "$NEW_FIN" ) && $OLD_IDX == "$NEW_IDX" ]]; then
  echo "move-web-state: already done ($WEBSTATE)"
  exit 0
fi
WEB=$(flag_of "$CHAIN-web-main" -listen)
[[ -n $WEB ]] || die "can't read the web server's -listen"
health() { curl -s -o /dev/null --max-time 30 -w '%{http_code}' "http://$WEB/api/health" || true; }
BEFORE=$(health)
[[ $BEFORE == 200 || $BEFORE == 503 ]] || die "the web server answers $BEFORE now; fix that first"

# stopped stops the units and checks every one is down: nothing may write
# the registry or the index while they move.
stopped() {
  systemctl stop "${UNITS[@]}" || return 1
  local u
  for u in "${UNITS[@]}"; do
    systemctl is-active --quiet "$u" && return 1
  done
  return 0
}

BACKUP=$(mktemp -d)
for u in "${UNITS[@]}"; do cp -p "$(unit_file "$u")" "$BACKUP/$u.service"; done
moved=() # "from|to", in the order moved
ok=0
cleanup() {
  [[ $ok == 1 ]] && return
  echo "move-web-state: failed; putting the files and units back" >&2
  if ! stopped; then
    echo "move-web-state: a unit won't stop, so nothing is moved back; moved so far: ${moved[*]:-nothing}; units are as edited" >&2
    return
  fi
  local i from to
  for ((i = ${#moved[@]} - 1; i >= 0; i--)); do
    IFS='|' read -r from to <<<"${moved[$i]}"
    if [[ -e $from ]]; then
      echo "move-web-state: $from exists again; leaving $to where it is" >&2
    else
      mv "$to" "$from"
    fi
  done
  for u in "${UNITS[@]}"; do cp -p "$BACKUP/$u.service" "$(unit_file "$u")"; done
  systemctl daemon-reload
  systemctl start "${UNITS[@]}" || true
}
trap cleanup EXIT

stopped || die "a unit didn't stop; nothing moved"
install -d -m 700 "$WEBSTATE"
[[ -n ${MOVE_TEST:-} ]] || chown "$CHAIN:$CHAIN" "$WEBSTATE"
move() { # from, to
  [[ $1 == "$2" || ! -e $1 ]] && return 0
  [[ -e $2 ]] && die "$2 already exists; not overwriting it"
  mv "$1" "$2"
  moved+=("$1|$2")
}
move "$OLD_REG" "$NEW_REG"
[[ -n $OLD_IDX ]] && move "$OLD_IDX" "$NEW_IDX"
[[ -n $OLD_FIN ]] && move "$OLD_FIN" "$NEW_FIN"
for u in "${UNITS[@]}"; do
  python3 - "$(unit_file "$u")" "-deposits $OLD_REG" "-deposits $NEW_REG" "-doge-index $OLD_IDX" "-doge-index $NEW_IDX" <<'PY'
import re, sys
path, pairs = sys.argv[1], sys.argv[2:]
text = open(path).read()
for old, new in zip(pairs[0::2], pairs[1::2]):
    text = re.sub(re.escape(old) + r"(?=\s|$)", new, text)
with open(path, "w") as f:
    f.write(text)
PY
done
python3 - "$(unit_file "$CHAIN-web-main")" "$NEW_FIN" <<'PY'
import re, sys
path, fin = sys.argv[1], sys.argv[2]
lines = open(path).read().split("\n")
for i, line in enumerate(lines):
    if line.startswith("ExecStart="):
        if re.search(r" -finality \S+", line):
            lines[i] = re.sub(r" -finality \S+", " -finality " + fin, line)
        else:
            lines[i] = line + " -finality " + fin
with open(path, "w") as f:
    f.write("\n".join(lines))
PY
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
echo "move-web-state: done. The registry is $NEW_REG, the finality samples $NEW_FIN${OLD_IDX:+, the index $NEW_IDX}; now deploy/sandbox-units.sh"
