# Separate signers

The DOGE locked on Dogecoin and the reserve on DogecoinVM are both held by an
m-of-n multisig of peg signers. This page covers running those signers
separately: each key on its own machine, each signer checking every
transaction for itself before signing.

## How it works

```
             proposals (unsigned tx + what it is for)
 coordinator ─────────────────────────────────────────▶ signer 1 ─┐
 (dogevm bridge,                                         signer 2 ─┼─ each: one key, own nodes,
  no keys)   ◀───────────────────────────────────────── signer 3 ─┘  own registry, signing log
             signatures
```

The coordinator is `dogevm bridge` running with `-cosigners`. It holds no
keys. It watches both chains, builds each transaction the bridge owes, and
asks the signers to sign it. With `Required` signatures it completes the
transaction and broadcasts it.

Each signer runs `dogevm signer` with one key. For every proposal it:

1. **Reads both chains itself**, through the nodes it is configured with, and
   runs the same audit as the bridge. If the peg is not fully backed, it
   signs nothing.
2. **Checks that the action is owed:**
   - **Release** (credit a deposit): the deposit is confirmed, has not been
     credited, and fits under the caps.
   - **Payout** (pay a withdrawal): the peg-out is final on DogecoinVM and
     has not been paid.
   - **Refund**: the deposit is held, and its operator has approved this
     refund to this address.
3. **Rebuilds the transaction.** It rebuilds the transaction for that action
   from the proposal's inputs, using its own view of their values, and signs
   only if the proposal matches byte for byte. The coordinator cannot change
   an amount, a destination or the fee.
4. **Checks its signing log.** A second transaction for an action it has
   already signed must spend one of the same outputs as each earlier one that
   could still confirm. At most one can then confirm, so a deposit can't be
   credited twice through this signer.
5. **Applies its daily limit**, if one is set (`-max-daily`).

A compromised coordinator can therefore delay transfers but can't move
locked DOGE. That would take `Required` signers.

## Running a signer

Each operator does this on their own machine.

1. **Make a key.** Share only the public key it prints.

   ```sh
   dogevm signer-key -out /var/lib/dogevm-signer/signer.key
   ```

   The key file is mode 0600 and must never leave the machine or be
   committed. The repository's hooks and CI refuse it; see [Secrets](#secrets).

2. **Build the signer set.** One person builds it from all the public keys
   and gives the same file to everyone. It holds no private keys.

   ```sh
   dogevm signers -required 2 -public-keys PUB1,PUB2,PUB3 -out signers.json
   ```

3. **Run nodes.** Run a Dogecoin Core node (`-txindex`, with a wallet for
   watch-only addresses) and a DogecoinVM node. Signers that share the
   coordinator's nodes are only as independent as those nodes.

4. **Start the signer.** Its policy flags must match the coordinator's:
   `-confirmations`, `-vm-fee`, `-doge-fee`, `-min-deposit`, `-min-peg-out`,
   `-max-deposit` and `-max-circulating`. Otherwise it rebuilds different
   transactions and refuses to sign.

   ```sh
   dogevm signer -signers signers.json -key-file /var/lib/dogevm-signer/signer.key \
     -listen 10.0.0.2:9700 -token-file /var/lib/dogevm-signer/token \
     -max-daily 50000000000 -confirmations 20 -max-deposit 10000000000 ...
   ```

   Listening on anything but loopback requires `-token-file`. Put the signer
   behind TLS or a private network (WireGuard, or an SSH tunnel). The token
   only keeps out strangers: the signer trusts nothing the coordinator says
   that it can't check.

5. **Back up the signing log** (`-log`, which defaults to `signing-log.json`
   next to the key). If the log is lost, the signer falls back on what the
   chains show.

### Refunds

A signer approves a refund only if the refund is listed in its
`-refund-approvals` file, one per line:

```
# deposit            refund to
TXID:VOUT DOGECOIN-ADDRESS
```

Each operator adds the line after checking the refund themselves. Then
`dogevm refund` on the coordinator gathers the signatures.

## Running the coordinator

The coordinator runs the bridge as usual, with a signer set that holds public
keys only, plus a list of signers:

```sh
dogevm bridge -signers signers.json -cosigners /var/lib/dogevm/cosigners.json ...
```

The list looks like this:

```json
[
  {"url": "https://signer1.internal:9700", "token": "…"},
  {"url": "https://signer2.internal:9700", "token": "…"},
  {"url": "https://signer3.internal:9700", "token": "…"}
]
```

`cosigners.json` holds tokens, so it stays with the deployment. When the web
wallet registers a personal deposit address, the coordinator tells every
signer straight away, so their nodes watch the address before anything is
sent to it. A signer that misses one picks it up from the next proposal that
involves it. If a deposit had already arrived by then, that signer's node
needs a rescan: restart it once with `dogevm signer -rescan`.

## Moving an existing deployment

To separate a deployment whose keys sit together in one signer set, without
changing the peg address:

1. Split the private keys into one key file per signer, on the machine that
   holds the set, and move each file to its signer's machine over SSH.
2. Replace the set everywhere with a public-keys-only copy, then delete every
   remaining copy of the combined set, including backups.
3. Start the signers, then restart the bridge with `-cosigners`.

Keys that were once stored together were exposed together. The stronger
step is to rotate: make new keys on each signer's machine and move the
locked DOGE and the reserve to a new signer set. The bridge doesn't support
moving to a new set yet; that is the next piece of work.

## Secrets

The repository must never contain private keys, tokens or passwords.
`make hooks` installs git hooks that scan every commit and push with
`scripts/secretscan`. CI scans every commit as well, and GitHub push
protection is on. Key files, signer sets, `cosigners.json`, signing logs,
tokens and `.env` files are also in `.gitignore`.
