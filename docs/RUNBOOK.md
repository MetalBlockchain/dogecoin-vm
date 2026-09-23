# Runbook

What to do when something goes wrong with the mainnet bridge. The commands
assume the host set up by `deploy/mainnet.sh`: state in `/var/lib/metal-main`,
secrets in `/var/lib/metal-main/secrets`, services run as the `dogevm` user.

```sh
S=/var/lib/metal-main/secrets
dv() { sudo -u dogevm env HOME=/opt/dogevm $(cat $S/bridge.env | xargs) /opt/dogevm/bin/dogevm "$@"; }
```

## First: pause

In any incident where funds might be at risk, pause first and investigate
second. A pause is safe: nothing is lost, and deposits and withdrawals made
while paused are processed after it ends.

```sh
dv pause -signers $S/signers.json -reason "Investigating an alert. Funds are safe."
```

Within seconds:
- the bridge stops proposing and paying anything, and refunds are refused;
- metaldoge.com shows a red banner with the reason, and `/api/status` has
  `paused`;
- the monitor sends a Telegram alert, and repeats it every 6 hours while
  paused.

To resume, once the cause is understood and fixed:

```sh
dv resume -signers $S/signers.json
```

With separate signers, each operator can also pause their own signer:
`dogevm pause -dir /var/lib/dogevm-signer -reason "..."`. Once fewer than
the required number of signers are available, nothing moves. A pause is the
one power every operator has on their own.

**What a pause does not do.** It stops the software. It does not stop
someone who has stolen the keys. While all three signer keys are on one
server, a compromise of that server exposes them all, and the beta caps
(100 DOGE per deposit, 1,000 in total) bound the loss. Separate signers
(Phase 2) are the real fix.

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
dv refund -signers $S/signers.json -list     # held or not yet credited, and why
```

- **Waiting for confirmations** (20): nothing to do.
- **Waiting for room under the circulating cap:** it's credited when there is room, or refund it.
- **Above the maximum deposit, below the minimum, or no destination:** refund it.

```sh
dv refund -signers $S/signers.json -deposit TXID:VOUT            # back to the sender
dv refund -signers $S/signers.json -deposit TXID:VOUT -to DOGEADDR
```

With separate signers, each operator first adds `TXID:VOUT DOGEADDR` to their
`-refund-approvals` file.

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
   rotation exists (Phase 2).

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
