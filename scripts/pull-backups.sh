#!/usr/bin/env bash
# Copies the encrypted backups from a DogecoinVM host to this machine, an
# off-server copy. They stay encrypted; only the age identity opens them.
#
#   scripts/pull-backups.sh root@HOST [DIR]     # DIR: ~/DogecoinVM-backups
set -euo pipefail
host=${1:?usage: $0 root@HOST [DIR]}
dir=${2:-$HOME/DogecoinVM-backups}
mkdir -p "$dir" && chmod 700 "$dir"
rsync -a ${SSH_KEY:+-e "ssh -i $SSH_KEY"} "$host:/var/backups/dogevm/" "$dir/"
ls -lh "$dir" | tail -n 5
