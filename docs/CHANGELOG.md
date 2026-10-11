# Changelog

What changed in DogecoinVM, the bridge at metaldoge.com and the Mac wallet,
newest first. DogecoinVM is **alpha**: see the [terms](https://metaldoge.com/terms).

Security fixes are listed once they're live, described by what they protect
rather than how the problem could be used.

## 11 Oct 2026: hardened bridge (v0.2.0)

The bridge and its three signers now run hardened software. Deposits and
withdrawals paused for about 80 minutes during the upgrade; DogecoinVM payments
kept working, and no balance changed.

- **Stricter signers.** Each signer keeps a permanent record of what it has
  signed and refuses anything that conflicts with it, checks every request
  against its own view of both chains, and stops if any of its history is
  missing.
- **A clean start.** One transaction on each chain moved the bridge's coins back
  to itself, so nothing signed under the old software can be used any more.
- **Limits on fees** paid from the bridge's own funds, and refunds only after a
  confirmed cancel.
- **Deposits wait at least 6 Dogecoin confirmations** (about 6 minutes), however
  small: one confirmation can be undone by an ordinary Dogecoin reorg.
- The site's services run with tighter system sandboxing.
- **Also in this release**, thanks to [@MlennGarien](https://github.com/MlennGarien):
  public RPC hardening (#4, live on the node since 26 Sep), signer-set
  fingerprints read as 24 words from the full hash (#3), and signer
  connections pinned to the signed-off set, with new passkey backups requiring
  PIN or biometric verification (#7). And DIP 21, `dogecoin:` and
  `dogecoinvm:` payment links, drafted by
  [@qlpqlp](https://github.com/qlpqlp) (#8).

## 9 Oct 2026: developer docs

- /developers: the public `/rpc` login is read-only. Broadcast with
  `POST /api/tx`. `searchrawtransactions` returns up to 500 results per call;
  page with `skip`.

## 8 Oct 2026: DogecoinVM v0.1.2

- The release node runners should build: the plugin mainnet runs.
  It's v0.1.1 plus the public-RPC fix: the public login can no longer
  crash the node with an oversized request, broadcast, rescan or load filters.
- /run-a-node points at v0.1.2. Don't run v0.1.1.

## 4 Oct 2026: DogecoinVM v0.1.1, developer pages

- v0.1.1 tagged: crash-safe block storage (blocks are on disk before they're
  accepted) and safer shutdowns.
- New /developers (every API endpoint, examples, building a wallet) and
  /run-a-node pages.

## 2 Oct 2026: Mac wallet 0.2.4

- A signer-set change no longer locks the wallet.
- Back up before receiving; restore from the backup key.

## 1–2 Oct 2026: alpha

- The bridge is labelled **alpha**, with terms of use and a banner saying who
  holds the keys.
- Caps: 100 DOGE per deposit, 4,200 DOGE in total.

## 27 Sep 2026: separate signers

- Each of the bridge's three signers runs as its own service with its own key,
  and signers only accept requests over mutually authenticated connections.
- Each request names the signer it's for and can't be replayed. A signer refuses
  to sign if its signing record is missing or out of date.

## 26 Sep 2026: node fixes

- Joining nodes can bootstrap the chain from peers.
- Mac wallet: DOGE or USD display, an address book, fees shown before sending.

## 25 Sep 2026: faster bridge

- Small deposits credit after fewer Dogecoin confirmations (tiers up to 1, 10
  and 50 DOGE).
- Payouts are sent straight away; the home page shows live finality.
- Wallets get new blocks pushed to them instead of polling.

## 24 Sep 2026: DogecoinVM v0.1.0, mainnet

- First round trip on mainnet: DOGE in, DogecoinVM payments, DOGE out.
- Double-sign protection fixed (thanks @MlennGarien, #2).
- Mac wallet 0.2.0 released (open source).
