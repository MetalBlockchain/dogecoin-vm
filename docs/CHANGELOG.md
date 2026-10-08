# Changelog

What changed in DogecoinVM, the bridge at metaldoge.com and the Mac wallet,
newest first. DogecoinVM is **alpha**: see the [terms](https://metaldoge.com/terms).

Security fixes are listed once they're live, described by what they protect
rather than how the problem could be used.

## In the code, not yet released

- **Public RPC hardening** (thanks [@MlennGarien](https://github.com/MlennGarien), #4).
  The fix the live node has run since 26 Sep, now in the public code with a test.
- **Signer-set fingerprints use the full hash** (thanks @MlennGarien, #3).
  Operators confirm a signer set by reading 24 words instead of a short code.
- **Signer connections and passkey backups** (thanks @MlennGarien, #7).
  A signer's connection must match the key in the signed-off signer set, and new
  wallet-site passkey backups require PIN or biometric verification. Older
  backups still open.

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
