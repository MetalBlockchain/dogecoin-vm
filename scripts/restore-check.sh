#!/usr/bin/env bash
# Proves a DogecoinVM backup restores: decrypts it into a temporary
# directory, checks every file against the manifest, and checks the restored
# keys are the ones in use:
#
#   - the peg signer set's private keys match it, and it controls the live
#     peg address;
#   - the staking certificate gives the validator's NodeID;
#   - the P-Chain key file matches its address.
#
#   scripts/restore-check.sh BACKUP.tar.age [AGE_IDENTITY]
#
# AGE_IDENTITY defaults to ~/.config/dogevm-backup/identity.txt. Restored
# files are deleted when the check finishes; nothing is left in plain text.
# Set LIVE_URL (default https://metaldoge.com) to compare with a live bridge.
set -euo pipefail

backup=${1:?usage: $0 BACKUP.tar.age [AGE_IDENTITY]}
identity=${2:-$HOME/.config/dogevm-backup/identity.txt}
live=${LIVE_URL:-https://metaldoge.com}
repo=$(cd "$(dirname "$0")/.." && pwd)

work=$(mktemp -d)
chmod 700 "$work"
cleanup() {
  # Overwrite restored files before removing them.
  find "$work" -type f -exec sh -c 'f=$1; dd if=/dev/urandom of="$f" bs=1 count="$(wc -c <"$f")" conv=notrunc 2>/dev/null' _ {} \; 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

fails=0
pass() { printf '  ok    %s\n' "$*"; }
fail() { printf '  FAIL  %s\n' "$*"; fails=$((fails + 1)); }

echo "Restoring $(basename "$backup")"
age -d -i "$identity" "$backup" | tar -C "$work" -xzf -
root=$work/dogevm
manifest=$root/MANIFEST.json
[[ -f $manifest ]] && pass "decrypted and unpacked; made on $(jq -r .host "$manifest") at $(jq -r .time "$manifest")" ||
  { fail "no manifest"; exit 1; }

# Every file matches the checksum taken when the backup was made.
if (cd "$root" && jq -r .sha256 MANIFEST.json | sha256sum -c --quiet - 2>/dev/null ||
  jq -r .sha256 MANIFEST.json | shasum -a 256 -c --quiet -); then
  pass "$(jq -r .sha256 "$manifest" | grep -c .) files match their checksums"
else
  fail "checksums do not match"
fi

bin=$work/bin
(cd "$repo" && go build -o "$bin/dogevm" ./cmd/dogevm && go build -o "$bin/dogevm-l1" ./cmd/dogevm-l1)

secrets=$root/var/lib/metal-main/secrets
# The signer set: keys match, and it controls the peg address in use.
if check=$("$bin/dogevm" signers-check -signers "$secrets/signers.json" -doge-network mainnet -vm-network mainnet 2>&1); then
  peg=$(jq -r .dogecoinPegAddress <<<"$check")
  pass "signer set: $(jq -r .privateKeys <<<"$check") private keys, each matching a public key"
  want=$(jq -r .dogecoinPegAddress "$manifest")
  [[ $peg == "$want" ]] && pass "signer set controls the backed-up peg address $peg" || fail "signer set controls $peg, not $want"
  if livepeg=$(curl -s -m 10 "$live/api/info" | jq -r '.pegAddress // empty') && [[ -n $livepeg ]]; then
    [[ $peg == "$livepeg" ]] && pass "and the live bridge's peg address ($live)" || fail "live peg address is $livepeg, not $peg"
  fi
else
  fail "signer set: $check"
fi

# The validator's staking identity.
nodeid=$("$bin/dogevm-l1" node-id -cert "$root/var/lib/metal-main/node/staking/staker.crt" 2>&1) || true
want=$(jq -r .nodeID "$manifest")
[[ -n $want && $nodeid == "$want" ]] && pass "staking certificate is validator $nodeid" || fail "staking certificate gives '$nodeid', manifest says '$want'"
[[ -s $root/var/lib/metal-main/node/staking/staker.key && -s $root/var/lib/metal-main/node/staking/signer.key ]] &&
  pass "staking and BLS signer keys present" || fail "staking or BLS signer key missing"

# The P-Chain key.
paddr=$("$bin/dogevm-l1" addresses -key "$secrets/p-chain-key.json" 2>/dev/null | awk '/^P-Chain/ {print $2}') || true
want=$(jq -r .pChainAddress "$secrets/p-chain-key.json")
[[ -n $paddr && $paddr == "$want" ]] && pass "P-Chain key controls $paddr" || fail "P-Chain key gives '$paddr', file says '$want'"

# Everything else a rebuild needs.
for f in secrets/deposits.json secrets/bridge.env secrets/telegram-token chain.json genesis.json; do
  [[ -s $root/var/lib/metal-main/$f ]] && pass "$f" || fail "$f missing"
done
[[ -s $root/var/lib/dogecoin-main/wallet.dat ]] && pass "Dogecoin watch-only wallet" || fail "Dogecoin wallet missing"
[[ -s $root/etc/caddy/Caddyfile ]] && pass "web server config" || fail "web server config missing"
units=$(find "$root/etc/systemd/system" -name '*.service' 2>/dev/null | wc -l | tr -d ' ')
[[ $units -ge 4 ]] && pass "$units service definitions" || fail "only $units service definitions"

if [[ $fails -eq 0 ]]; then
  echo "Restore check passed."
else
  echo "Restore check FAILED: $fails problem(s)."
  exit 1
fi
