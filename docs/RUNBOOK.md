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

Fees paid from the surplus (a cancellation's, a cutover fence's) need room: each signer signs
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
6. compares coverage: `dogevm coverage` on the coordinator and on every
   signer (the registry's hash, and every unsettled coin of the set on both
   chains) must match, or it stops here with nothing started (a cutover
   records only what its node sees); then checks each signer's signing
   log;
7. starts a cutover (see "Cutover") for every signer whose log is
   unproven: adopted, or written by software from before the cutover rule,
   whatever its anchors; and checks its nodes see the coordinator's tips.
   A proven log (a new key's, or one that has been through a cutover)
   gets its empty anchors set, and must lead, along confirmed spends, to
   those tips (see "Anchor lineage");
8. fills in each signer's public key in `cosigners.json` from its
   `card.json` if missing, checking every key is in the signer set;
9. crosses the point of no return, **then** starts the signers, still
   paused, and the web wallet and monitor (the site shows the pause); the
   bridge stays stopped. **From here nothing is rolled back**, a signer
   that won't start included: signers start one by one, the first may sign
   a fence before a later one fails, and the old binary knows nothing of
   the cutover gate. A failure from here on keeps the new binaries and
   every log, leaves the bridge stopped and every signer paused, and says
   so;
10. while any current-set signer is unproven or has coins left in a
    cutover (see "Cutover"; later epochs included), sends fences on both
    chains with the bridge unit's own command line plus `-fence` (signers
    sign fences while paused, and nothing else), every `FENCE_WAIT_POLL`
    seconds (default 60), for up to `FENCE_WAIT` minutes (default 360;
    `0`: one round). Each round first sends the set's pending
    transactions out again (a node that dropped a fence from its mempool
    gets it back). A signer is done when its `cutoverLeft` is `{}` and its
    `history` is `fenced` (or `new`: a key that never signed); `signer-log
    status` on a stopped signer finishes a cutover whose coins are all
    cleared. If time runs out it stops with exit status 1, the bridge
    stopped, everything paused, and prints each unfinished signer's
    `history` and `cutoverLeft`, the commands to send more fences, and how
    to read each signer's `signer-log status`;
11. when every signer is done, starts the bridge, takes fresh health
    (`/api/health`: every expected check present: pause, peg, bridge,
    dogecoin, dogecoinvm) and checks the peg is solvent.

It resumes only if `RESUME=1`, the bridge wasn't paused before the
upgrade, and every check but the pause passes; then it lifts the bridge's
pause and each signer pause it made. Otherwise read `/api/health` and
resume the signers and the bridge with the commands it prints.

A failure **before step 9** puts the previous binaries, `cosigners.json`
and contribution lists back, starts everything again and leaves it paused.
It never puts an older signing log back over a newer one: a log only grows,
and an older copy would forget a signature. A log this run created,
unchanged since, is removed; any other log that changed during the run is
kept, with a warning: find out what that signer signed before resuming it.

A signer with **no signing log** stops the upgrade: restore the log from a
backup. The only exceptions are named explicitly:

- `INIT_LOGS="dogevm-signer-N"` for a key that never signed;
- `ADOPT_LOGS="dogevm-signer-N ..."` for a key that signed only before
  separate signers existed, so every signature it made is on the chains.
  It rebuilds the log from them, anchors included.

The three DogecoinVM signers set up in September 2026 signed without logs,
so the first upgrade adopts them, and then fences both chains: adoption
sees only what reached a chain, not a signature kept back. Follow "First
upgrade of the live bridge" below, step by step.

After an upgrade to this version, while the bridge is still paused, move
the web server's files out of the secrets directory once, then sandbox the
services, before resuming:

    deploy/move-web-state.sh     # the registry, finality samples and coin index, to /var/lib/metal-main/web
    deploy/sandbox-units.sh

The web server then writes only that directory, never the secrets
directory (the pause file, the signer set, `cosigners.json`,
`contributions.txt`). `move-web-state.sh` stops the bridge, web wallet and
monitor and checks they are down before it moves anything, never
overwrites a file already at a destination (a half-done earlier run: sort
out which copy is newer by hand), and on a failure moves back exactly what
it moved. `deploy/sandbox-units.sh remove` takes the sandbox away again,
and fails unless the bridge answers as it did before.

**Never run `deploy/provision.sh` on the mainnet host.** It is the testnet
provisioner: it rewrites the bridge to hold every key in one process and
restarts the node. It refuses to run where it finds Metal mainnet or
separate signers.

Before the first upgrade to the anchor rule, check the current set's peg
address holds a confirmed coin (`dv audit`, or the peg address on an
explorer); if not, contribute first, or payouts wait.

## First upgrade of the live bridge

The live DogecoinVM bridge (Metal mainnet, Dogecoin mainnet) runs software
from before the cutover rule; its three signers kept no logs. Its first
upgrade adopts their logs from the chains and fences both chains before
anything else is signed. Do these in order, as root on the bridge's host;
stop at the first surprise, and stay paused until the last step. Nothing
here touches the VM plugin (the Metal node keeps running it); that is
upgraded separately.

Every command uses the files the bridge unit itself runs with, never a
guessed path:

    eval "$(deploy/bridge-paths.sh)"   # SET, REG, COS from dogevm-bridge-main's command line
    audit() { dv audit -signers "$SET" -deposits "$REG"; }

Read them again after step 10 (`move-web-state.sh` moves the registry).

1. **Pin the code.** Choose the commit, run on a workstation
   `go test ./cmd/dogevm/ ./vm/...` with Dogecoin Core actually running
   (`DOGECOIND_DOCKER=...`: the Core tests must run; a skip fails this
   step) and every `deploy/test/*.sh` under bash 5. Push the branch to
   `origin` (an operator's decision) and record the commit hash. Build the
   new binary on the host without installing it, for the read-only checks
   below (it reads; it signs nothing):

       NEW=/opt/dogevm/bin/dogevm-preflight   # somewhere the service user can run it, not /root
       sudo -u dogevm env HOME=/opt/dogevm PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local \
         bash -c "cd /opt/dogevm/src && git fetch && git checkout <commit> && go build -o $NEW ./cmd/dogevm"
       dvn() { sudo -u dogevm env HOME=/opt/dogevm $(cat $S/bridge.env | xargs) "$NEW" "$@"; }
       dvn version   # must run as the service user before you go on

   (Delete `$NEW` after step 13; the upgrade installs its own build.)

2. **Pause everything first: the bridge and every signer** (round 8):
   a dishonest coordinator ignores its own pause, so the signers must
   refuse too.

       dv pause -signers "$SET" -reason "Upgrading."
       for n in 1 2 3; do
         runuser -u dogevm-signer-$n -- bash -c "set -a; . /var/lib/dogevm-signer-$n/signer.env; set +a; \
           /opt/dogevm/bin/dogevm pause -signers /var/lib/dogevm-signer-$n/signers.json -reason Upgrading."
       done

   From here nothing is credited, paid, refunded or signed. (The upgrade
   finds these pauses and keeps them; you lift them by hand in step 11.)
3. **Inventory and operating capital.** List every coin of the set on
   both chains, with the new software, read-only:

       dvn inventory -signers "$SET" -deposits "$REG"

   It shows each coin's value and depth, how many fences they take (each
   fence after the first starts with the change the one before paid back,
   so 200 coins at 100 a fence is three fences), what those cost (a
   Dogecoin fence pays `-doge-fee`, live 0.1 DOGE, per started 10 kB, plus
   headroom for four replacements of the costliest one, each one fence fee
   more), the surplus, and what the surplus must be: **twice** the fences'
   fees and headroom, as each signer promises at most half of it and pays
   at most half of it over the whole cutover. If the surplus is short,
   contribute `needed` minus the surplus **plus a margin of one Dogecoin
   fence fee per batch**, with the `DVMC` tag ("Operating
   capital") and **wait for 20 confirmations** (rootDepth), not 6. Run it
   again: the contribution must show in the surplus (the old software,
   still running, would have called it a held payment; the new one counts
   it as capital).
4. **Every set coin deep, nothing pending, nothing conflicted.**
   `dvn inventory -require-ready -signers "$SET" -deposits "$REG"` must
   succeed: every coin at the peg address and the personal deposit
   addresses at least 20 confirmations deep, no payment to the set
   unconfirmed on either chain (round 9: wait for it, or for what replaces
   it), no transaction of the set conflicted short of 20, and the surplus
   enough. The upgrade runs the same check before adopting anything, and
   stops (exit 3) if it fails; if `inventory` couldn't run at all (a node
   down, a wrong path) that is exit 1, not 3.
5. **Reconcile against the chain, outpoint by outpoint (stop gate).** The
   signers kept no logs, so nothing records what they signed before; the
   cutover can only fence what the wallets know. Check that what they know
   is everything: for every address `inventory` lists under `addresses`
   (the peg address and every registered personal deposit address), list
   the address's unspent outputs and its transaction history on a
   Dogecoin full node or explorer independent of this host, and compare
   with `inventory`'s coins:
   - every unspent output there is in `inventory`, and every `inventory`
     coin is unspent there, with the same value;
   - no transaction paying one of these addresses is unconfirmed, or was
     seen and then dropped (an explorer showing it double-spent, or a
     wallet backup that has it and the live wallet doesn't): any such one
     must be conflicted at least 20 deep, or the deploy waits;
   - the wallet backups (`/var/backups/dogevm`, and the copies off the
     host) list no transaction to these addresses the live wallets lack.
   **Any unexplained difference stops the deploy.** Don't adopt until it
   is explained and fixed (register the address everywhere, rescan, or
   wait for depth).
6. **Coverage.** Every node must cover the same coins: the same registry
   and the same wallet history. On each signer, register every address the
   coordinator's registry has (`dogevm deposit-address -signers <its
   signers.json> -deposits <its deposits.json> ...` as that signer's user,
   or copy `$REG` there while it is stopped), and rescan any wallet short
   of the others (`getwalletinfo` `txcount`). The upgrade compares them
   itself (`dogevm coverage` on the coordinator and every signer: the
   registry's hash and every unsettled coin of the set on both chains)
   before adopting, and again right after the cutovers start (each
   signer's recorded coins against the coordinator's coverage); either
   difference stops it (exit 2). Don't work around that stop: a coin one
   signer can't see is a coin its cutover won't record.
7. **Backup.** `dogevm-backup run`, then copy it off the host
   (`scripts/pull-backups.sh`).
8. **Upgrade, adopt, fence.** Without `RESUME`, so you resume by hand, and
   with every signer adopted (none of these keys may use `INIT_LOGS`:
   they have signed):

       BRANCH=<the pushed branch> ADOPT_LOGS="dogevm-signer-1 dogevm-signer-2 dogevm-signer-3" \
         deploy/upgrade-bridge.sh <commit>

   It runs "Upgrading the bridge software": coverage compared, inventory
   checked, the three logs adopted and cut over and their recorded coins
   compared again, the point of no return crossed **before any signer
   starts**, the bridge left stopped, and fences sent on both chains until
   every signer is done (a Dogecoin fence needs 20 confirmations, about 20
   minutes, and DogecoinVM fences wait until it has them; more coins than
   one fence holds means more rounds). Its exit status says where it
   stopped:
   - **0**: done; everything still paused.
   - **2**: a node's coverage, or a signer's recorded coins, differ from
     the coordinator's. Nothing started; the previous binaries are back
     and running, paused (their pause files stay; the old software signs
     nothing while paused). Fix the difference (step 6) and run again.
   - **3**: not ready to adopt (the `inventory` problems are printed).
     Same state as 2. Fix them (steps 3–5) and run again.
   - **4**: the cutover isn't finished in `FENCE_WAIT`. Expected for a big
     peg: run the fence commands it printed until **every** signer's
     `signer-log status` shows `cutoverLeft` `{}` **and** `history`
     `"fenced"`. If one signer is still gated, don't resume the others:
     find the coin it waits for (`dogevm unsettled list`, "Cutover"). It
     also prints the start, health and resume commands, for step 9 on.
   - **1**: anything else, including an `inventory` that couldn't run;
     read the message. Before the point of no return it is the same state
     as 2, except that a signer whose pause couldn't be put back is left
     **stopped**, not started (round 9); after it, nothing is rolled back
     (below). Either way the script lists each signer as running paused,
     stopped paused, stopped unpaused, or STILL RUNNING AND NOT PAUSED:
     the last means stop it by hand, now.
9. **Check while still paused.** On a block explorer, every coin a
   signer recorded is spent, or its transaction conflicted, at least 20
   confirmations deep (DogecoinVM: in a block). Each signer's
   `signer-log status`: fenced, its `anchors` equal to the coordinator's
   `lineageTips`, read with the bridge's own files:

       dv signer-log anchors -signers "$SET" -deposits "$REG"

   No "quarantin" in `journalctl -u dogevm-signer-N`. Then
   `systemctl start dogevm-bridge-main`; `curl -s
   http://127.0.0.1:8081/api/health` has the `pause`, `peg`, `bridge`,
   `dogecoin` and `dogecoinvm` checks, only the pause failing; `audit` is
   solvent, the surplus lower only by the fence fees `inventory` predicted.
10. **Web state and sandbox, still paused.** `deploy/move-web-state.sh`,
    then `deploy/sandbox-units.sh`. Then `eval "$(deploy/bridge-paths.sh)"`
    again (the registry moved), open the site, make a deposit address, and
    check `/api/health` again: still only the pause failing.
11. **Resume**, with the commands the upgrade printed: each signer first,
    then the bridge.
12. **Smoke test, small amounts.** One deposit (a few DOGE) to a personal
    deposit address and its credit; one withdrawal and its payout; restart
    one signer (`systemctl restart dogevm-signer-2`) and check it comes
    back fenced; `audit` after each.
13. **Backup again** (`dogevm-backup run`) and copy it off the host: the
    signers' new logs and each node's `dogecoin-unsettled.json` are now
    worth keeping.

If anything fails once the upgrade has crossed its point of no return
(just before the signers start), nothing rolls back: the new binaries and
every log stay, the bridge stays stopped, and every signer paused. Read the
error, and `signer-log status` on each signer, before doing anything else.

## Anchor lineage

Every payout, refund, release, cancellation and fence spends an **anchor**
first, so any two transactions the signers sign on one chain conflict, or
one descends from the other: a payout signed and kept back can't confirm
beside a second one.

The anchor is the tip of a **line** of coins, one per chain: at the peg
address on Dogecoin, at the reserve on DogecoinVM. The line starts at its
**root**: of the outputs ever paid to that address with full
confirmations, the oldest one the signers have spent, or, before they have
spent any, the oldest of all (on Dogecoin a contribution, see "Operating
capital"; on DogecoinVM the reserve's first coin). Preferring a spent coin
means a payment shown to a node late, in an old block, can't take the line
over: nobody has spent it. The line moves only along confirmed spends: each
transaction in a block that spends the line's coin moves it to that
transaction's last output to the same address, its change. Coins anyone
else pays there (deposits, contributions, peg-outs) never become the anchor
unless one is the root; they are spent beside it. Every signer reading the
same chain finds the same line. BTCVM and LTCVM use the same rule.

Each signer's log keeps the anchor it last signed with, and moves on only
by following the line forward from it. A log with **no anchor** signs
nothing: a signer that picked its own, from a wallet whose history differed
from the others' (missing an old payment, or rescanned since), could follow
a line of its own. Set it, with the signer stopped, from that signer's own
nodes:

    dv signer-log anchors -set -signers SET -key-file KEY -log LOG

and check the `walked` tips it prints (where its anchors lead) are the
`tips` the coordinator sees (`dv signer-log anchors -signers SET` with the
bridge's settings) and the other signers': `deploy/upgrade-bridge.sh` does
this for every proven signer. A new signer set does this once its first
contribution has full confirmations. A log that is unproven goes through a
cutover instead (see "Cutover"), which ends with its anchors at the tips.

If the trail breaks, the signer refuses with "anchor ... is gone, but no
confirmed transaction it knows spends it", or "the line ended". That
happens when:

- **A reorganisation removed the transaction that created or spent the
  anchor.** Wait: when the chain settles, the transactions are usually mined
  again and the trail is whole.
- **The signer's wallet is missing history** (restored, or a new node).
  Rescan the signer's Dogecoin Core wallet (`rescan`), then retry.
- **A confirmed transaction spent the anchor and paid nothing back** (only
  history from before every transaction kept change). The line, read from
  history, starts again at a later coin; a signer's log never follows that
  by itself.

A signer whose `anchors -set` reports a tip different from the others' has
a wallet that sees a different history: rescan it, and don't let it sign
until the tips agree.

If the trail stays broken, the recovery is a cutover, never moving the
anchor alone: with the signer stopped,

    dv signer-log cutover -signers SET -key-file KEY -log LOG

starts a new one (see "Cutover"). Its fences spend every coin the set holds,
the old anchor too if it is still unspent, so nothing this signer signed
from it can confirm afterwards; when it is done, the anchor is the tip.
Never hand-edit a signer's `anchors` entry.

## Cutover

A signature already given stays valid as long as every coin its
transaction spends is unspent. A log rebuilt from the chains
(`ADOPT_LOGS`, `signer-log adopt`) holds only what reached them; a log from
software before this rule may hold signatures that spend any coin the set
had (the live DogecoinVM signers signed for months without logs or an
anchor line). Matching anchors don't kill such a signature. So such a log
is **unproven** (`history` empty in `signer-log status`) and goes through a
**cutover** before it signs anything else, on either chain: an action has
a transaction on each chain (a release on DogecoinVM, a refund on
Dogecoin), and a new one on a finished chain could confirm beside an old
one kept back on the other.

The cutover records, per chain, every coin a signature given before could
spend: every output to the set's addresses (the peg address and every
registered personal deposit address on Dogecoin, the reserve on
DogecoinVM) in any transaction its wallet lists, in a block, in the
mempool, or conflicting with one in a block; and every input of every
transaction in its log. It leaves out what is already spent deep. A coin is
**cleared** when a transaction spends it with full confirmations
(DogecoinVM: in a block), or when its transaction is conflicted by one that
deep. The cutover is done when every recorded coin on both chains is
cleared: then the log's `history` is `fenced` and its anchors are the
lines' tips. Until then it refuses everything but fences with "in a cutover
... signs only fences".

It starts by itself: adopt starts one, and an unproven log starts one on its
first status or sign request. `signer-log cutover` starts a new one by hand
(a new epoch; the coins of an unfinished one stay recorded).
`signer-log status` shows `history`, `cutoverEpoch`, `cutoverLeft` (coins
not yet cleared, per chain), `cutoverPending` (coins nothing in a block
spends yet), `anchors` and `lineageTips`.

The coordinator spends the recorded coins with **fences**:

    dv bridge <the bridge unit's flags> -fence dogecoin
    dv bridge <the bridge unit's flags> -fence dogecoinvm

(`deploy/upgrade-bridge.sh` runs these with the unit's own command line.) A
fence spends the line's tip, then every confirmed coin the set holds on
that chain that nothing spends, oldest first (at most 100 on Dogecoin, 400
on DogecoinVM), all to one output at the set's own address, less a fee,
tagged `DVMZ`. Its inputs come from the chain, not from any signer's record,
so two signers whose cutovers began at different times (one before a
deposit arrived, one after) build the same fence; a signer's record decides
only whether it still needs one, and it refuses a fence that spends none of
its recorded coins. The coordinator proposes while any signer reports coins
left. Fences:

- are signed while paused (they pay no one), and only inside a cutover:
  never once it is done, however often asked;
- wait while anything else of the set is pending on that chain, while a
  fence of this cutover is pending, and while one is in a block short of
  full confirmations: a set with more coins than one fence holds fences in
  turn, about 20 minutes apart on Dogecoin;
- pay, from the surplus and under the half-surplus rule (contribute first
  if a signer refuses for room): on Dogecoin one `-doge-fee` per started
  10 kB of the signed fence; on DogecoinVM `max(vm fee, ceil(size × vm fee
  / 250))`, so a big reserve's fence still clears the relay floor;
- are replaced on Dogecoin when one has waited `-fence-bump-after`
  (default 30 minutes): the same coins, one fence fee more each time, as
  long as the signers' half-surplus share allows (no fixed count; a
  DogecoinVM fence is final in its block and is only resent);
- count only if they spend a recorded coin: a stranger's payment tagged
  `DVMZ` neither holds the next fence up nor clears anything;
- on DogecoinVM, wait for the Dogecoin part (round 9: two phases): a
  signer pays nothing else from the surplus (DogecoinVM fences,
  cancellations) while its own Dogecoin cutover still lists a coin. Its
  Dogecoin coins clear only once spent, or their transaction conflicted,
  20 deep, and every Dogecoin fence spends the line's tip first, so by
  then every Dogecoin fence anyone signed against them, sent or kept back,
  is in a block deep or dead. It also waits while it sees any Dogecoin
  fence of the set unconfirmed, short of 20, or listed as conflicted
  short of 20 (Core lists a fence it dropped from its mempool at -1; that
  one is sent again, it isn't dead);
- count against a **budget** over the whole cutover (round 9): each
  signer pays at most its half of the surplus it saw when it first priced
  a fence in the cutover, every fence it signed counted once at its
  costliest alternative, confirmed or not. Every signed transaction
  carries an honest signature, so the fees of every quorum together stay
  within the surplus. A signer that runs out refuses ("more than its
  budget"): contribute (DVMC, 20 deep), check on an explorer that every
  Dogecoin fence of the cutover so far is 20 deep or dead (a new epoch
  gives a new budget, which must not stack on a fence another quorum may
  still have kept back), then `signer-log cutover` on that signer.

Deposits aren't affected: what is owed is read from each deposit's own
transaction, not from its coin staying where it was paid.

A recorded coin whose transaction a reorganisation took out of its block
can't be fenced until it is mined again; normally that takes minutes.
There is **no waiver**: a transaction's absence from the blocks or from a
mempool proves nothing, since anyone holding it can still have it mined
(round 6). Such a coin keeps the cutover gated, which is safe: the signer
signs nothing but fences. Recover by finding out what the transaction was
(`signer-log status` lists the coin; `dogevm unsettled list -signers <the
node's signers.json>` shows each unsettled transaction and its evidence),
then get it, or a conflicting spend of its inputs, into a block. Once that
is full confirmations deep (20) the coin clears by the normal rule:

- if the wallet lists the conflicting spend (it pays the set), on its own;
- if it was mined after all and the wallet lost it and the spend of its
  output too: prove that spend the same way (`-spender` is the
  transaction spending the recorded output); the node puts it in its
  history at its block's depth, and the coin clears once that is 20 deep
  (round 9);
- if it doesn't (a payer double-spent a deposit to themselves), record
  the spend on **every** node that holds the dropped transaction:
  `dogevm unsettled prove -signers <its signers.json> -txid <the dropped
  transaction> -spender <the spending transaction's hex> -block <the hash
  of its block>`, as that node's user. Nothing in a proof is trusted: each
  node checks, on every read, that the spender spends one of the dropped
  transaction's inputs and that its own chain holds it in that block, and
  how deep. A wrong proof clears nothing. A coinbase is never evidence:
  every coinbase spends the same null input.

The dropped transaction's record stays in the unsettled file for good: it
is what clears its outputs, so deleting it would leave them owed (round 8).
Only evidence read now clears a coin (round 9): the wallet listing a
conflicting spend, or a proof that still checks out. A record "settled"
because the wallet once listed it conflicted 20 deep clears nothing once
the wallet stops listing that, so prove it. If a reorganisation makes the
evidence shallow again, the coin shows as owed again until it is deep
again.

An old log's `cutoverWaived` coins (software that had a waiver) go back
into its cutover when it loads, even a log that had finished: they clear
only the normal way.

Every Dogecoin wallet transaction that isn't in a block with full
confirmations is kept, body and all, in `dogecoin-unsettled.json` beside the signer set: the
coordinator's in `/var/lib/metal-main/secrets/separate-signers/`, each
signer's in `/var/lib/dogevm-signer-N/`. The bridge and the signers write
it; the web server and the monitor can only read it
(`deploy/sandbox-units.sh` makes it read-only for them, and refuses a web
directory that would contain it). Dogecoin Core 1.14 doesn't report what a
reorganisation removed, and rereads the whole wallet after one; a
transaction the wallet stops listing is put back, as conflicted at the
depth of the best current evidence it can never be mined (a spend of one of
its inputs, read fresh each time), so the cutover still covers its coins,
across restarts too, until that evidence is 20 deep.
The daily backup includes it (it backs up the secrets directory and every
signer's directory); don't delete it.

A coin at a personal deposit address only some signers watch is recorded
only by them, and the fences, built from the coordinator's view, don't
spend it: those signers stay in their cutover (fail closed). Register the
address with the coordinator (`dv deposit-address`), then start a new
epoch (`signer-log cutover`) on the signers that finished, so they sign the
fence that spends it. Before a cutover, `dogevm coverage` on every node
must match (the upgrade checks it and stops otherwise): that is what the
cutover's coverage rests on.

If a signer is quarantined during the cutover (the chains show a
transaction it signed that its new log lacks), someone sent a kept-back
signature: stop, and find out what it was, before anything else.

Cancellations spend the reserve's anchor first and, when it alone can't pay
the fee and leave change, the largest other reserve coins it needs. A
signer that signed a release of a deposit may sign its cancellation when
that release spends the same anchor (the two conflict); the refund that
follows still waits until the cancellation is in a block and every release
the signer signed is dead.
