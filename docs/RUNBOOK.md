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

Any operator can also pause **every** signer, without the coordinator:
`dogevm pause -dir /var/lib/dogevm-signer -remote all -key-file
/var/lib/dogevm-signer/signer.key -reason "..."` signs the pause with their
signer key and sends it to each signer on the set's cards (or `-remote
URL,URL`), and says which it reached. A signer applies a pause signed by a
key of its set, dated within 10 minutes of its clock and after it was last
resumed. Only each signer's own operator resumes it, with `dogevm resume
-dir`, so a stolen operator key can stop the peg but never restart it.

**What a pause does not do.** It stops the software. It does not stop
someone who has stolen the keys. While all three signer keys are on one
server, a compromise of that server exposes them all. The beta caps (100
DOGE per deposit, 1,000 in total) do **not** bound that loss: they are
enforced by the signer software, not by the peg's script, so whoever holds
two keys can move everything locked, held deposits included. The caps bound
what a bug or a misbehaving coordinator can do. Separate signers (Phase 2)
are the real fix.

## Alerts

The monitor checks every minute and alerts on Telegram when a check changes.
`curl -s https://metaldoge.com/api/health` shows the same checks.

| Check | Failing means | Do |
| --- | --- | --- |
| `peg` | Locked DOGE no longer covers circulating DOGE plus pending transfers. The bridge has already stopped itself. | Pause. Run `dv audit -signers $S/signers.json`, and compare proof of reserves on the site with a public Dogecoin explorer. Don't resume until the difference is explained. |
| `pause` | The bridge is paused. | Expected during an incident. Confirm who paused it and why (`cat $S/paused.json`). |
| `bridge` | A deposit or withdrawal is well past due; a payout or refund has waited unconfirmed for `-stall-blocks` minutes (every later payout waits behind it, since each spends the peg's anchor); a deposit is held and owed back; or one waits for room under the cap. | `journalctl -u dogevm-bridge-main -n 50`. A stuck payout: the bridge hands it back to Dogecoin Core every 10 minutes; check `dogecoin-cli getmempoolentry TXID` and the node's peers. A held deposit: refund it (below). Common causes otherwise: a node behind, or signers unreachable. |
| `wallet` | Dogecoin Core's wallet holds 100,000 or more transactions. The bridge stops at a million rather than read a truncated history (it would miss old payouts and pay them again). | Find what is filling it (dust sent to deposit addresses?). |
| `dogecoin` | Dogecoin Core is down or behind. | `systemctl status dogecoind-main`; restart it if stopped. While it syncs, deposits wait and nothing is lost. |
| `dogecoinvm` | The DogecoinVM node isn't answering. | `systemctl status metal-mainnet`, and its logs. |
| `validator` | The validator's P-Chain balance is low; at zero the validator stops and the chain halts. | Top up the validator balance on the P-Chain. This isn't scripted yet, so do it before the balance runs out; the alert gives the days left. |

## A deposit that hasn't arrived

```sh
dv refund -signers $S/signers.json -list     # held or not yet credited, and why
```

- **Waiting for confirmations** (20, or fewer for small deposits under the
  tiers, never fewer than 6 on mainnet): nothing to do. Small deposits
  credited early are capped in total (`-max-shallow`, by default the
  largest tier's amount); the rest wait for all 20.
- **Waiting for room under the circulating cap:** it's credited when there is room, or refund it.
- **Above the maximum deposit, below the minimum, or no destination:** refund it.

```sh
dv refund -signers $S/signers.json -deposit TXID:VOUT            # back to the sender
dv refund -signers $S/signers.json -deposit TXID:VOUT -to DOGEADDR
```

With separate signers, each operator first adds `TXID:VOUT DOGEADDR` to their
`-refund-approvals` file.

A deposit that is still **creditable** (waiting for room under the cap)
needs `force` at the end of each operator's approval line, and `-force` on
the command, which then takes two runs:

1. The first cancels the deposit on DogecoinVM: a small transaction that
   spends the reserve's anchor (its fee comes from operating capital). Any
   release of the deposit that someone signed and kept back spends that same
   anchor, or an earlier one already spent, so once the cancellation is in a
   block (final on DogecoinVM) no release of the deposit can ever confirm,
   and no signer will sign one.
2. Run it again once the cancellation is in a block: the deposit is now
   held, and is refunded like any held deposit, once it has its
   confirmations.

A signer that signed a release of the deposit refuses both steps; the other
two can still do them.

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

1. Provision a new Ubuntu 24.04 server. Install Go, metalgo, DogecoinVM and
   Dogecoin Core by hand, following the install steps of
   `deploy/provision.sh` but **not running it** (it's the testnet
   provisioner, and refuses on a mainnet host), then the node setup in
   `deploy/mainnet.sh`.
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

## Operating capital (contributions)

Every payout and refund spends the oldest coin at the signer set's own peg
address first (the anchor), so any two the signers sign conflict and at most
one confirms; every payout pays change of at least 0.01 DOGE back to the peg
address, which becomes the next anchor. A payout waits while the one before
it is unconfirmed. A new set's peg address holds nothing until something is
paid to it, so it needs one contribution before its first payout.

The peg's surplus is only what operators deliberately contribute. Deposits
that weren't credited, and payments into the reserve that weren't valid
peg-outs, are owed back to whoever sent them, so they count as liabilities,
never surplus. To contribute, pay the peg address with an `OP_RETURN` of
`DVMC` (hex `44564d43`) and nothing else, for example with
`createrawtransaction '[]' '{"PEG_ADDRESS": 1, "data": "44564d43"}'`, then
fund, sign and send it from a Dogecoin Core wallet. It is never credited and
never refunded, and `/api/audit` shows it as `contributed`. A payment to the
peg address without a tag is a deposit without a destination: held, and owed
back as a refund.

## Upgrading the bridge software

`deploy/upgrade-bridge.sh COMMIT` (as root) upgrades the coordinator and
every separate signer together, since they must speak the same request
format, and touches nothing else: the Metal node and Dogecoin Core keep
running. It:

1. builds the commit;
2. pauses the bridge with a public reason (keeping a pause already there);
3. backs up the binaries, every signing log and `cosigners.json`;
4. stops the bridge and signers, installs the new binary, and checks each
   signer's signing log;
5. fills in each signer's public key in `cosigners.json` from its
   `card.json` if missing, checking every key is in the signer set;
6. copies `contributions.txt` to every signer;
7. starts everything, and checks the peg is solvent.

It leaves the bridge paused unless `RESUME=1` (and it did the pausing):
read `/api/health`, then resume. On any failure it puts the previous
binaries, logs and `cosigners.json` back and leaves the bridge paused.

A signer with **no signing log** stops the upgrade: restore the log from a
backup. The only exceptions are named explicitly:

- `INIT_LOGS="dogevm-signer-N"` for a key that never signed;
- `ADOPT_LOGS="dogevm-signer-N ..."` for a key that signed only before
  separate signers existed, so every signature it made is on the chains.
  It rebuilds the log from them.

The three DogecoinVM signers set up in September 2026 signed only under the
old combined bridge, so the first upgrade adopts them:

    ADOPT_LOGS="dogevm-signer-1 dogevm-signer-2 dogevm-signer-3" deploy/upgrade-bridge.sh COMMIT

**Never run `deploy/provision.sh` on the mainnet host.** It is the testnet
provisioner: it rewrites the bridge to hold every key in one process and
restarts the node. It refuses to run where it finds Metal mainnet or
separate signers.

Before the first upgrade to the anchor rule, check the current set's peg
address holds a confirmed coin (`dv audit`, or the peg address on an
explorer); if not, contribute first, or payouts wait.

## Anchor lineage

Every payout, refund and release spends an **anchor** first: the latest
coin the signers themselves paid change to, at the peg address (Dogecoin)
or the reserve (DogecoinVM). So any two transactions the signers sign on one
chain conflict, and at most one can confirm. Before the signers have made
anything there, the anchor is the oldest coin with at least the full
confirmations (a contribution, see "Operating capital").

Each signer remembers the anchor it last signed with, and moves on only by
following confirmed spends forward from it: that anchor's confirmed spender,
then that spender's change at the same address, and so on. If the trail
breaks, the signer refuses with "anchor ... is gone, but no confirmed
transaction it knows spends it". That happens when:

- **A reorganisation removed the transaction that created or spent the
  anchor.** Wait: when the chain settles, the transactions are usually mined
  again and the trail is whole.
- **The signer's wallet is missing history** (restored, or a new node).
  Rescan the signer's Dogecoin Core wallet (`rescan`), then retry.

Never hand-edit a signer's `anchors` entry, unless you've confirmed on a
public explorer that the old anchor was spent by a signers' transaction in
a block deeper than any plausible reorganisation, and every transaction any
signer signed with it is that one or conflicts with it.
