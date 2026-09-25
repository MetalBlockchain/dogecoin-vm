# First round trip, then separate signers

What to do once the bridge's Dogecoin node has caught up: prove a deposit
and a withdrawal end to end on mainnet, then move the bridge onto separate
signer services. Commands run on the server, as root.

## 0. The node has caught up

The monitor sends "all checks OK" to Telegram when the `dogecoin` check
passes. To check by hand:

```sh
curl -s https://metaldoge.com/api/health | jq '.status, (.checks[] | select(.ok | not))'
```

`status` should be `ok`, with nothing failing. The Dogecoin index then
catches up too: the web wallet's Dogecoin balance appears.

Then give memory back: the sync ran with a 6 GB cache (`dbcache=6000` in
`/var/lib/dogecoin-main/dogecoin.conf`), which a synced node doesn't need on
a 15 GB server shared with metalgo and the bridge. Set `dbcache=1000` and
restart Dogecoin Core at a quiet moment (`systemctl restart dogecoind-main`;
it takes a minute or two to come back).

## 1. The first deposit is credited

The 1 DOGE deposit (Dogecoin transaction `74e053f6…`) is credited 20
confirmations after the node sees it, less the 0.01 DOGE bridge fee.

- In the wallet: the DogecoinVM balance shows 0.99 DOGE, and the Deposit tab
  lists the deposit as credited.
- In the explorer (`/explorer`): the deposit appears under Bridge activity
  with a transaction on each chain, and Proof of reserves lists the locked
  output.
- `curl -s https://metaldoge.com/api/status | jq .audit`: `locked` covers
  `circulating` plus anything pending, and the monitor's `peg` check passes.

If it isn't credited within a few minutes of 20 confirmations, see "A deposit
that hasn't arrived" in [RUNBOOK.md](RUNBOOK.md).

## 2. A withdrawal back to Dogecoin

From the Mac app (or the web wallet's Withdraw tab), withdraw at least the
minimum to your own Dogecoin address. Check the review screen: the amount to
the bridge, and the Dogecoin address it pays.

- The withdrawal is final on DogecoinVM in about two seconds; the bridge pays
  it on its next pass (every 30 seconds), less the 0.1 DOGE Dogecoin fee.
- The explorer shows the withdrawal with both transactions; the Dogecoin one
  links to a public explorer.
- `/api/status` audit still balances, and `/api/health` stays `ok`.

## 3. Record it

Note both round trips' transaction IDs (deposit, credit, withdrawal, payout),
then update the roadmap: "Where it stands", and Phase 1's done items, with
links. That's the end-to-end proof.

## 4. Separate signers

This moves the bridge onto the Phase 2 design on this server: three signer
services, one key each, each checking every transaction against the nodes
before signing, and a coordinator that holds no keys. The peg address
doesn't change. It's a rehearsal of the protocol with live funds; the
security gain comes when signers move to other operators' machines
([SIGNERS.md](SIGNERS.md)).

Do it at a quiet moment: nothing pending in either direction
(`/api/status` audit `pendingPegIns` and `pendingPegOuts` are 0).

### Stage

```sh
cd /opt/dogevm/src && sudo -u dogevm git pull -q
deploy/stage-signers.sh
```

It copies each key to its own signer directory under
`/var/lib/metal-main/secrets/separate-signers`, assembles and joins the set,
checks the keys and their order match the live set's, and writes a service
file per signer. It starts nothing. `rm -r` the directory undoes it.

Staged again on 25 September 2026 with the confirmation tiers in the policy
(fingerprint `7c71-1377-b369-6632-ecd5`, peg address unchanged). Before starting the signers, refresh each one's copy
of the deposit registry, since addresses registered after staging aren't in
it:

```sh
S=/var/lib/metal-main/secrets/separate-signers
for n in 1 2 3; do sudo -u dogevm cp /var/lib/metal-main/secrets/deposits.json $S/signer$n/deposits.json; done
```

### Start the signers

```sh
S=/var/lib/metal-main/secrets/separate-signers
for n in 1 2 3; do cp $S/signer$n/dogevm-signer-$n.service /etc/systemd/system/; done
systemctl daemon-reload
systemctl enable --now dogevm-signer-1 dogevm-signer-2 dogevm-signer-3
for n in 1 2 3; do
  sudo -u dogevm bash -c "set -a; . $S/signer$n/signer.env; exec /opt/dogevm/bin/dogevm signer-setup check -dir $S/signer$n"
done
```

Every check should pass, including that each signer refuses an unsigned
request.

### Switch the bridge

In `/etc/systemd/system/dogevm-bridge-main.service`, change `-signers` in
`ExecStart` and add the coordinator flags, keeping the policy flags as they
are:

```
-signers /var/lib/metal-main/secrets/separate-signers/signers.json
-cosigners /var/lib/metal-main/secrets/separate-signers/cosigners.json
-coordinator-key-file /var/lib/metal-main/secrets/separate-signers/coordinator/coordinator.key
```

Point the web and monitor services' `-signers` at the same public set. Then:

```sh
systemctl daemon-reload
systemctl restart dogevm-bridge-main dogevm-web-main dogevm-monitor-main
journalctl -u dogevm-bridge-main -u dogevm-signer-1 -u dogevm-signer-2 -u dogevm-signer-3 -f
```

### Prove it

Make a small deposit and a small withdrawal again. Each signer's log shows
it checking and signing; `signing-log.json` in each signer directory records
what it signed. Health stays `ok`.

### Retire the combined key file

Once a backup has run with the signer directories in it (`dogevm-backup run`,
then `scripts/restore-check.sh` on your Mac), move
`/var/lib/metal-main/secrets/signers.json` out of service and delete it,
including from old backups when they age out. From then on each key exists
only in its signer's directory and in the backups.

### Roll back

Stop the signers, put the old `-signers` path back in the three services,
drop the coordinator flags, and restart. The peg address never changed, so
nothing on either chain needs to move.

```sh
systemctl disable --now dogevm-signer-1 dogevm-signer-2 dogevm-signer-3
```
