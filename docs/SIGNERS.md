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

## The setup ceremony

`dogevm signer-setup` walks everyone through setup. Only public information
changes hands: no private key, token or password is ever sent to anyone.
Every step asks its questions in a terminal. With `-yes` it takes everything
from flags instead, for scripts and agents, and refuses rather than guesses
when something is missing.

| Who | Step | What it does |
| --- | --- | --- |
| Each operator | `signer-setup init` | Makes a key on this machine, or imports one (pasted at a hidden prompt, or read with `-import-key-file` or `-import-key-stdin`, never from a flag). Writes a **signer card**: name, URL, public key, and a signature proving the operator holds the key. |
| Coordinator | `signer-setup coordinator` | Makes the coordinator key, which signs every request to the signers. |
| Coordinator | `signer-setup assemble CARD...` | Checks every card and builds the **signer set**: the keys, how many must sign, the networks, the coordinator key and the bridge policy (confirmations, fees, caps). Prints its **fingerprint**. |
| Each operator | `signer-setup join` | Shows the set and its fingerprint, which each operator confirms with the others over a separate channel. Then writes the service file and a settings file for the operator's own nodes. |
| Anyone | `signer-setup check` | Checks the key file, set membership, both nodes (and that Dogecoin is synced), and that the signer service is up and refusing unsigned requests. |

A typical run:

```sh
# Each operator, on their own machine
dogevm signer-setup init            # asks: name, URL, make a key or import one
# → sends /var/lib/dogevm-signer/card.json to the coordinator

# The coordinator
dogevm signer-setup coordinator
dogevm signer-setup assemble -required 2 -coordinator-key PUB \
  -doge-network mainnet -vm-network mainnet \
  -confirmations 20 -max-deposit 10000000000 -max-circulating 100000000000 \
  alice.json bob.json carol.json
# → sends signers.json to every operator, and reads the fingerprint out on a call

# Each operator
dogevm signer-setup join -signers signers.json   # confirm the fingerprint, set a daily limit
#   then fill in signer.env with this machine's own node settings
sudo cp /var/lib/dogevm-signer/dogevm-signer.service /etc/systemd/system/
sudo systemctl enable --now dogevm-signer
dogevm signer-setup check
```

For an agent or script, the same steps take flags. For example:

```sh
dogevm signer-setup init -yes -dir /var/lib/dogevm-signer -name "Example Pool" \
  -url https://signer.example.com:9700            # makes a new key
dogevm signer-setup join -yes -dir /var/lib/dogevm-signer -signers signers.json \
  -fingerprint 3f9a-02bc-7d41-e0a8-55c1 -max-daily 500
```

Without a terminal, `join` needs `-fingerprint`, and it must be the one the
other signers read out. An agent can't confirm a fingerprint for itself.

### What each operator runs

- **The key**, which never leaves the machine. Back it up offline.
- **A Dogecoin Core node** (`-txindex`, with a wallet for watch-only
  addresses) and **a DogecoinVM node**. These are what make the signer
  independent: it checks everything against its own nodes. A signer that
  uses someone else's node trusts that node's operator.
- **The signer service** from `join`. It serves on port 9700. Requests must
  be signed by the coordinator key and be no more than 5 minutes old. The
  requests and answers carry only public data, so TLS is good practice but
  not what keeps funds safe.
- **The signing log** (`signing-log.json`). Back it up. If it is lost, the
  signer falls back on what the chains show.

The policy is part of the signer set, so the coordinator and every signer
use the same one. A policy flag that disagrees with it is an error.
Changing the policy, such as raising a cap, means a new set that every
operator joins again.

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

The coordinator runs the bridge with the set, the list of signers from
`assemble`, and its key:

```sh
dogevm bridge -signers signers.json -cosigners cosigners.json \
  -coordinator-key-file /var/lib/dogevm/coordinator.key ...
```

When the web wallet registers a personal deposit address, the coordinator
tells every signer straight away, so their nodes watch the address before
anything is sent to it. A signer that misses one picks it up from the next
proposal that involves it. If a deposit had already arrived by then, that
signer's node needs a rescan: restart it once with `dogevm signer -rescan`.

## Moving an existing deployment

To separate a deployment whose keys sit together in one signer set, without
changing the peg address:

1. Give each operator one of the existing keys over a secure channel. Each
   runs `signer-setup init` and pastes the key at the hidden prompt, or uses
   `-import-key-file`. Keep the cards in the old set's key order, so the
   peg address stays the same.
2. Run `assemble` and `join` as above, then delete every copy of the old
   combined set, including backups.
3. Start the signers, then restart the bridge with the new set,
   `-cosigners` and `-coordinator-key-file`.

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
