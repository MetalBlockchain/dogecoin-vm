#!/usr/bin/env bash
# Installs the signers deploy/stage-signers.sh staged, each as its own
# system user, so no process but that signer can read its key:
#
#   /var/lib/dogevm-signer-N         the signer's key, signing log and settings
#                                   (mode 700, owned by dogevm-signer-N)
#   dogevm-signer-N.service          runs /usr/local/lib/dogevm/dogevm, a copy
#                                   owned by root, so the dogevm user (which
#                                   runs the web server) can't replace it
#
# Dogecoin Core 1.14 has one wallet per node, so the signers share the node's
# watch-only wallet over RPC; each still checks every proposal itself.
#
# It then starts the signers and checks each. It doesn't touch the running
# bridge: the switch to coordinator mode is in docs/FIRST-ROUND-TRIP.md
# (deploy/mainnet.sh launch writes it once $STAGE exists).
#
#   sudo deploy/install-signers.sh
#
# Settings, from the environment:
#   STAGE   where stage-signers.sh staged them
#   DOGEVM  the dogevm binary to copy
set -euo pipefail

STAGE=${STAGE-/var/lib/metal-main/secrets/separate-signers}
DOGEVM=${DOGEVM-/opt/dogevm/bin/dogevm}
BIN=/usr/local/lib/dogevm/dogevm

die() { echo "install-signers: $*" >&2; exit 1; }

[[ $(id -u) == 0 ]] || die "run as root"
[[ -x $DOGEVM ]] || die "can't run $DOGEVM"
shopt -s nullglob
staged=("$STAGE"/signer[0-9]*/)
staged=("${staged[@]%/}")
[[ ${#staged[@]} -ge 1 ]] || die "nothing staged in $STAGE; run deploy/stage-signers.sh first"

install -d -o root -g root -m 755 "$(dirname "$BIN")"
install -o root -g root -m 755 "$DOGEVM" "$BIN"

for src in "${staged[@]}"; do
  n=${src##*/signer}
  user=dogevm-signer-$n
  dir=/var/lib/$user
  [[ -e $dir ]] && die "$dir already exists; stopped before touching it"
  unit=$src/dogevm-signer-$n.service
  [[ -f $unit ]] || die "no service file in $src"

  id "$user" >/dev/null 2>&1 ||
    useradd --system --no-create-home --home-dir "$dir" --shell /usr/sbin/nologin "$user"
  mv "$src" "$dir"
  chown -R "$user:$user" "$dir"
  chmod 700 "$dir"

  # The staged unit, moved to the new directory, user and binary.
  sed -e "s#$src#$dir#g" -e "s#^User=.*#User=$user#" -e "s#^ExecStart=[^ ]*#ExecStart=$BIN#" \
    "$dir/dogevm-signer-$n.service" >"/etc/systemd/system/$user.service"
  grep -q "^ReadWritePaths=$dir\$" "/etc/systemd/system/$user.service" ||
    die "$user.service has no ReadWritePaths=$dir; check $dir/dogevm-signer-$n.service"
  echo "installed $user"
done

systemctl daemon-reload
for src in "${staged[@]}"; do
  n=${src##*/signer}
  systemctl enable --now "dogevm-signer-$n" >/dev/null
done
sleep 5

failed=0
for src in "${staged[@]}"; do
  n=${src##*/signer}
  user=dogevm-signer-$n
  echo "== $user"
  # A Dogecoin node still syncing fails its check; the others must pass.
  # The check exits non-zero while a node syncs; the verdict below allows
  # that, so its status is not what counts (pipefail would make it).
  { sudo -u "$user" bash -c "set -a; . /var/lib/$user/signer.env; set +a
    exec $BIN signer-setup check -dir /var/lib/$user" 2>&1 || true; } |
    python3 -c '
import json, sys
text = sys.stdin.read()
bad = 0
for c in json.loads(text[text.index("["):text.rindex("]") + 1]):
    print(("  ok  " if c["ok"] else "  BAD ") + c["check"] + ": " + c["detail"])
    bad += not c["ok"] and not c["detail"].startswith("still syncing")
sys.exit(bad)' || failed=1
done
[[ $failed == 0 ]] || die "a check failed"
echo
echo "Signers running. The switch to coordinator mode is in docs/FIRST-ROUND-TRIP.md."
