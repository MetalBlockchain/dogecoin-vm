# Runbook

What to do when something goes wrong with the mainnet bridge. The commands
assume the host set up by `deploy/mainnet.sh` and moved onto separate signers
([FIRST-ROUND-TRIP.md](FIRST-ROUND-TRIP.md)), with services running as the
`dogevm` user.

```sh
SECRETS=/var/lib/metal-main/secrets
S=$SECRETS/separate-signers
dv() { sudo -u dogevm env HOME=/opt/dogevm $(cat $SECRETS/bridge.env | xargs) /opt/dogevm/bin/dogevm "$@"; }
# The coordinator's flags, for commands that ask the signers to sign
co="-signers $S/signers.json -cosigners $S/cosigners.json -coordinator-key-file $S/coordinator/coordinator.key"
```

Older copies of this runbook used `$SECRETS/signers.json`. That file no
longer exists, so commands that name it fail. If the bridge's unit passes
`-deposits`, pass the same to `audit` and `refund`.

## First: pause

In any incident where funds might be at risk, pause first and investigate
second. A pause is safe: nothing is lost, and deposits and withdrawals made
while paused are processed after it ends.

```sh
dv pause -signers $S/signers.json -reason "Investigating an alert. Deposits and withdrawals are on hold."
```

This writes `$S/paused.json`, next to the set the bridge, web wallet and
monitor run with. Within seconds:
- the bridge stops proposing and paying anything, and refunds are refused;
- metaldoge.com shows a red banner with the reason, and `/api/status` has
  `paused`;
- the monitor sends a Telegram alert, and repeats it every 6 hours while
  paused.

To resume, once the cause is understood and fixed:

```sh
dv resume -signers $S/signers.json
```

Each signer can also be paused on its own:

```sh
sudo -u dogevm /opt/dogevm/bin/dogevm pause -dir /var/lib/dogevm-signer-N -reason "..."   # N = 1, 2 or 3
sudo -u dogevm /opt/dogevm/bin/dogevm resume -dir /var/lib/dogevm-signer-N
```

Once fewer than the required number of signers are available, nothing
moves. With independent operators, a pause is the one power every operator
has on their own. A pause that other operators can trigger remotely isn't in
the live bridge yet: each pause is written where its process runs.

**What a pause does not do.** It stops the software. It does not stop
someone who has stolen the keys. During the alpha one operator holds all of
the bridge's signing keys. The alpha caps (100 DOGE per deposit, 4,200 DOGE
in total) and each signer's daily limit (4,200 DOGE) are enforced by the
signer software, so they don't limit what someone holding the keys could
take. The signers already run as separate services; the real fix is moving
the keys to independent operators (Phase 3 on the roadmap).

## Alerts

The monitor checks every minute and alerts on Telegram when a check changes.
`curl -s https://metaldoge.com/api/health` shows the same checks.

| Check | Failing means | Do |
| --- | --- | --- |
| `peg` | Locked DOGE no longer covers circulating DOGE plus pending transfers. The bridge has already stopped itself. | Pause. Run `dv audit -signers $S/signers.json`, and compare proof of reserves on the site with a public Dogecoin explorer. Don't resume until the difference is explained. |
| `pause` | The bridge is paused. | Expected during an incident. Confirm who paused it and why (`cat $S/paused.json`). |
| `bridge` | A deposit or withdrawal is well past due. | `journalctl -u dogevm-bridge-main -n 50`. Common causes: a node behind, the circulating cap reached (the deposit waits), or signers unreachable. |
| `dogecoin` | Dogecoin Core is down or behind. | `systemctl status dogecoind-main`; restart it if stopped. While it syncs, deposits wait and nothing is lost. |
| `dogecoinvm` | The DogecoinVM node isn't answering. | `systemctl status metal-mainnet`, and its logs. |
| `validator` | The validator's P-Chain balance is low; at zero the validator stops and the chain halts. | Top up the validator balance on the P-Chain. This isn't scripted yet, so do it before the balance runs out; the alert gives the days left. |

## A deposit that hasn't arrived

```sh
dv refund $co -list     # held or not yet credited, and why
```

- **Waiting for confirmations** (1 to 20, by size): nothing to do.
- **Waiting for room under the circulating cap:** it's credited when there is room, or refund it.
- **Above the maximum deposit, below the minimum, or no destination:** refund it.

Users ask for refunds through the contact details at
[metaldoge.com/terms#contact](https://metaldoge.com/terms#contact). Check that
the request comes from the sender before refunding anywhere but back to the
sender.

```sh
dv refund $co -deposit TXID:VOUT            # back to the sender
dv refund $co -deposit TXID:VOUT -to DOGEADDR
```

Each signer approves only refunds listed in its `-refund-approvals` file, so
first add `TXID:VOUT DOGEADDR` to the file named by `-refund-approvals` in
each signer's unit (`systemctl cat dogevm-signer-N`).

## Suspected compromise of the server

1. Pause.
2. Cut public access if needed: `systemctl stop dogevm-web-main`. The bridge
   and nodes keep running, paused.
3. Check what moved: proof of reserves on the site, and the peg address on a
   public Dogecoin explorer.
4. Take a fresh backup (`dogevm-backup run`), and keep the server as it is
   for investigation.
5. Rebuild on a new server from backup (below). Treat every key that was on
   the old server as exposed: move the funds to new keys as soon as key
   rotation exists (Phase 3).

## Restore from backup

Backups are made daily at 03:17 UTC to `/var/backups/dogevm`, encrypted to an
age key that only the operators hold. `scripts/pull-backups.sh` keeps a copy
off the server.

Prove a backup restores, on a machine with the age key:

```sh
scripts/restore-check.sh ~/DogecoinVM-backups/dogevm-YYYYMMDDTHHMMSSZ.tar.age
```

Rebuild a host from one:

1. Provision a new Ubuntu 24.04 server. Run the install parts of
   `deploy/provision.sh` (Go, metalgo, DogecoinVM, Dogecoin Core), and the
   node setup in `deploy/mainnet.sh`.
2. Copy the backup over and unpack it at the filesystem root. It holds its
   files at their original paths:

   ```sh
   age -d -i identity.txt dogevm-….tar.age | tar -xzf - -C /tmp && cp -a /tmp/dogevm/. / && rm -rf /tmp/dogevm
   chown -R dogevm:dogevm /var/lib/metal-main /var/lib/dogecoin-main
   ```

3. Start Dogecoin Core and the Metal node, and let both sync. The restored
   staking files keep the same NodeID, so the validator carries on.
4. Start the bridge, web wallet and monitor, **paused** until everything
   checks out: `dv pause …`, then start the services, `dv audit`, and resume.
5. Point DNS at the new server.

Only the restored secrets can move funds, so wipe the unpacked copy and
anything else holding them once the host is running.
