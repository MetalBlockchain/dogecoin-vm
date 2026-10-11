#!/usr/bin/env bash
# Prints, as shell assignments, the files the live bridge unit actually
# uses, read from its own command line, so operator commands use the same
# ones (RUNBOOK.md, "First upgrade of the live bridge"):
#
#   eval "$(deploy/bridge-paths.sh)"   # sets SET, REG, COS
#   dv audit -signers "$SET" -deposits "$REG"
#
# SET is the signer set (-signers), REG the deposit address registry
# (-deposits; without the flag, deposits.json next to SET, as dogevm reads
# it), COS the remote signers' list (-cosigners; empty without it). Read it
# again after move-web-state.sh: the registry moves.
set -euo pipefail
CHAIN=${CHAIN:-dogevm}
start=$(systemctl show -p ExecStart --value "$CHAIN-bridge-main")
flag() { sed -nE "s/.* $1 ([^ ;]+).*/\1/p" <<<"$start"; }
SET=$(flag -signers)
[[ -n $SET ]] || { echo "bridge-paths: $CHAIN-bridge-main has no -signers" >&2; exit 1; }
REG=$(flag -deposits)
COS=$(flag -cosigners)
printf 'SET=%q\nREG=%q\nCOS=%q\n' "$SET" "${REG:-$(dirname "$SET")/deposits.json}" "$COS"
