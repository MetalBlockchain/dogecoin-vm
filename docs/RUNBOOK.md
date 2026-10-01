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
| `bridge` | A deposit or withdrawal is well past due; a payout or refund has waited unconfirmed for `-stall-blocks` minutes (every later payout waits behind it, since each spends the peg's anchor); a deposit is held and owed back; or one waits for room under the cap. | `journalctl -u dogevm-bridge-main -n 50`. A stuck payout: the bridge hands it back to Dogecoin Core every 10 minutes, and after `-bump-after` (30 minutes) replaces it with one spending the same inputs and paying another `-doge-fee` (BIP125; at most four fees, never more than half the payment); check `dogecoin-cli getmempoolentry TXID` and the node's peers. A held deposit: refund it (below). Common causes otherwise: a node behind, or signers unreachable. |
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

A deposit with **no destination** (an untagged payment to the peg address)
can never be credited, and is refunded in one run.

A deposit that **names a destination** (a personal deposit address, or a
`DVMD` tag) could have been credited under some policy, and a release of it
signed and kept back could still confirm, whatever the limits say now. So
its refund takes two runs:

1. The first cancels the deposit on DogecoinVM: a small transaction that
   spends the reserve's anchor (its fee comes from operating capital). Any
   release of the deposit that someone signed and kept back spends that same
   anchor, or an earlier one already spent, so once the cancellation is in a
   block (final on DogecoinVM) no release of the deposit can ever confirm,
   and no signer will sign one.
2. Run it again once the cancellation is in a block: the deposit is now
   cancelled, and is refunded once it has its confirmations.

Signers refund such a deposit only after its cancellation is in a block. A
deposit that is still **creditable** (waiting for room under the cap)
also needs `force` at the end of each operator's approval line, and
`-force` on the command.

A signer that signed a release of the deposit signs neither step while that
release could still confirm; once the chain shows it can't (another
transaction in a block spends its anchor), it signs both. The other two can
always do them.

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

Every payout and refund spends the anchor first (see "Anchor lineage"), so
any two the signers sign conflict, or one descends from the other; every
payout pays change of at least 0.01 DOGE back to the peg address, which
becomes the next anchor. A payout waits while the one before it is
unconfirmed. A new set's peg address holds nothing until something is paid
to it, so it needs one contribution, with full confirmations, before its
first payout.

Fees paid from the surplus (a cancellation's) need room: each signer signs
one only if it, with the fees that signer already signed for and that could
still confirm, is at most half the surplus (2 of 3 signers). Two pairs of
signers that share a dishonest one then can't promise more than the
surplus between them. Keep at least a few DOGE contributed.

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

Top-ups made before the tag existed (untagged payments to the peg address)
would count as deposits owed back. List each such output, `TXID:VOUT`, one
per line, in `contributions.txt` next to the signer set: the coordinator's
(`secrets/separate-signers/`) and every signer's (`/var/lib/dogevm-signer-N/`).
Outputs, not transactions: a transaction ID alone is refused, since one
transaction can pay a top-up and a customer's deposit. List only an output
you can show was a top-up; listing a customer's deposit would hide a debt.
The files must match, or the signers' audits disagree with the
coordinator's. `deploy/upgrade-bridge.sh` copies the coordinator's to every
signer, beside the signer set its unit runs with, and checks they match.

## Upgrading the bridge software

`deploy/upgrade-bridge.sh COMMIT` (as root) upgrades the coordinator and
every separate signer together, since they must speak the same request
format, and touches nothing else: the Metal node and Dogecoin Core keep
running. It:

1. builds the commit;
2. pauses the bridge with a public reason (keeping a pause already there);
3. stops the bridge, web wallet and monitor; pauses every signer with its
   own pause file; stops every signer. Only then, with nothing able to sign,
   it backs up the binaries, every signing log, `cosigners.json` and the
   contribution lists;
4. installs the new binary;
5. copies the coordinator's `contributions.txt` beside every signer's set,
   and checks they match;
6. checks each signer's signing log;
7. sets each signer's empty anchors to the tips of the anchor lines as its
   own nodes see them, and checks every signer's anchors are the tips the
   coordinator sees (see "Anchor lineage");
8. fills in each signer's public key in `cosigners.json` from its
   `card.json` if missing, checking every key is in the signer set;
9. starts the signers (still paused), then the bridge, and checks the peg
   is solvent.

It leaves the bridge and the signers paused unless `RESUME=1`, it did the
pausing, and every check in `/api/health` but the pause passes; otherwise
read `/api/health` and resume the signers and the bridge with the commands
it prints. On a failure it puts the previous binaries, `cosigners.json` and
contribution lists back and leaves everything paused. It never puts an older
signing log back over a newer one: a log only grows, and an older copy would
forget a signature. A log this run created, unchanged since, is removed;
any other log that changed during the run is kept, with a warning: find out
what that signer signed before resuming it.

A signer with **no signing log** stops the upgrade: restore the log from a
backup. The only exceptions are named explicitly:

- `INIT_LOGS="dogevm-signer-N"` for a key that never signed;
- `ADOPT_LOGS="dogevm-signer-N ..."` for a key that signed only before
  separate signers existed, so every signature it made is on the chains.
  It rebuilds the log from them, anchors included.

The three DogecoinVM signers set up in September 2026 signed only under the
old combined bridge, so the first upgrade adopts them:

    ADOPT_LOGS="dogevm-signer-1 dogevm-signer-2 dogevm-signer-3" deploy/upgrade-bridge.sh COMMIT

After an upgrade to this version, move the web server's files out of the
secrets directory once, then sandbox the services:

    deploy/move-web-state.sh     # the registry and finality samples, to /var/lib/metal-main/web
    deploy/sandbox-units.sh

The web server then writes only that directory and its coin index, never
the secrets directory (the pause file, the signer set, `cosigners.json`,
`contributions.txt`).

**Never run `deploy/provision.sh` on the mainnet host.** It is the testnet
provisioner: it rewrites the bridge to hold every key in one process and
restarts the node. It refuses to run where it finds Metal mainnet or
separate signers.

Before the first upgrade to the anchor rule, check the current set's peg
address holds a confirmed coin (`dv audit`, or the peg address on an
explorer); if not, contribute first, or payouts wait.

## Anchor lineage

Every payout, refund and release spends an **anchor** first, so any two
transactions the signers sign on one chain conflict, or one descends from
the other: a payout signed and kept back can't confirm beside a second one.

The anchor is the tip of a **line** of coins, one per chain: at the peg
address on Dogecoin, at the reserve on DogecoinVM. The line starts at its
**root**, the oldest output ever paid to that address with full
confirmations (on Dogecoin a contribution, see "Operating capital"; on
DogecoinVM the reserve's first coin), and moves only along confirmed spends:
each transaction in a block that spends the line's coin moves it to that
transaction's last output to the same address, its change. Coins anyone
else pays there (deposits, contributions, peg-outs) never become the anchor
unless one is the root; they are spent beside it. Every signer reading the
same chain finds the same line.

Each signer's log keeps the anchor it last signed with, and moves on only
by following the line forward from it. A log with **no anchor** signs
nothing: a signer that picked its own, from a wallet whose history differed
from the others' (missing an old payment, or rescanned since), could follow
a line of its own. Set it, with the signer stopped, from that signer's own
nodes:

    dv signer-log anchors -set -signers SET -key-file KEY -log LOG

and check the `anchors` it prints are the `tips` the coordinator sees
(`dv signer-log anchors -signers SET` with the bridge's settings) and the
other signers': `deploy/upgrade-bridge.sh` does this for every signer. A
new signer set does it once its first contribution has full confirmations.

If the trail breaks, the signer refuses with "anchor ... is gone, but no
confirmed transaction it knows spends it". That happens when:

- **A reorganisation removed the transaction that created or spent the
  anchor.** Wait: when the chain settles, the transactions are usually mined
  again and the trail is whole.
- **The signer's wallet is missing history** (restored, or a new node).
  Rescan the signer's Dogecoin Core wallet (`rescan`), then retry.

A signer whose `anchors -set` reports a tip different from the others' has
a wallet that sees a different history: rescan it, and don't let it sign
until the tips agree.

Never hand-edit a signer's `anchors` entry, unless you've confirmed on a
public explorer that the old anchor was spent by a signers' transaction in
a block deeper than any plausible reorganisation, and every transaction any
signer signed with it is that one or conflicts with it.
