# DogecoinVM two-way peg

DogecoinVM never issues DOGE. Every DOGE on DogecoinVM is backed by DOGE locked on Dogecoin, and moves between the chains through a two-way peg run by a set of signers.

## Pieces

| Piece | Where | What it does |
|---|---|---|
| Peg reserve | DogecoinVM consensus ([`btcd/params.go`](../btcd/params.go), [`btcd/blockchain/validate.go`](../btcd/blockchain/validate.go)) | Blocks 1..`pegReserveBlocks` must each pay 9,000,000,000 DOGE to `pegReserveAddress`. This is the only way coins come into existence. |
| Signer set | [`cmd/dogevm/signers.go`](../cmd/dogevm/signers.go) | An m-of-n multisig. The reserve on DogecoinVM and the peg address on Dogecoin both use its redeem script, so they share one P2SH hash. |
| Bridge | [`cmd/dogevm/bridge.go`](../cmd/dogevm/bridge.go) | Watches both chains. It credits deposits from the reserve, pays peg-outs from the peg address, and refuses to act if the peg is not fully backed. |

The reserve is not circulating supply. It is locked to the signers and leaves the reserve only when DOGE of equal value is locked on Dogecoin. A single transaction may create at most 10 billion DOGE, so the reserve is spread across several blocks. Size it to Dogecoin's supply plus headroom: Dogecoin issues about 5.256 billion DOGE a year, so 50 blocks (450 billion DOGE) lasts decades.

## Flows

**Peg-in (Dogecoin → DogecoinVM)**
1. The user sends DOGE on Dogecoin to the peg address. The same transaction carries an `OP_RETURN` tagged `DVMD` naming the DogecoinVM address to credit (`dogevm peg-in` builds it).
2. After `-confirmations` Dogecoin blocks (default 6), the bridge spends the reserve. It pays the deposit amount, minus `-vm-fee` (default 0.01 DOGE), to that address, and tags the transaction `DVMI` with the deposit's outpoint.
3. DogecoinVM finalizes the release in its next block.

**Peg-out (DogecoinVM → Dogecoin)**
1. The user pays DOGE on DogecoinVM to the reserve address. The transaction carries an `OP_RETURN` tagged `DVMO` naming a Dogecoin address (`dogevm peg-out` builds it).
2. Once it is in a block (final), the bridge pays the amount, minus `-doge-fee` (default 1 DOGE), from the peg address on Dogecoin. It tags the payment `DVMR` with the peg-out's txid.

## Tags

Each is the whole data of the transaction's single `OP_RETURN` output. Transactions with more than one `OP_RETURN` are ignored.

| Tag | Chain | Payload | Bytes |
|---|---|---|---|
| `DVMD` | Dogecoin | type (0 = P2PKH, 1 = P2SH), hash160 | 25 |
| `DVMI` | DogecoinVM | deposit txid (internal byte order), vout (little endian) | 40 |
| `DVMO` | DogecoinVM | type, hash160 | 25 |
| `DVMR` | Dogecoin | peg-out txid | 36 |

## Safety properties

- **Exactly once.** The bridge keeps no database; it reads its state back from both chains every time. A deposit counts as credited only if a release carrying its outpoint exists, and a peg-out as paid only if a payment carrying its txid exists. After a crash, the bridge picks up where it left off.
- **Spoofed tags are ignored.** Anyone can write `DVMI` or `DVMR` into a transaction. The bridge only trusts them on transactions that spend reserve or peg outputs, which only the signers can do. Otherwise, a forged `DVMI` could mark someone's deposit as already credited.
- **Solvency check.** Before every action the bridge checks that `locked on Dogecoin ≥ circulating on DogecoinVM + pending peg-ins + pending peg-outs`, and halts if it does not hold. `dogevm audit` prints the same numbers for anyone to check.
- **Deposits without a usable tag** (none, malformed, or below `-min-deposit`) are not credited. They show up in the audit as `unclaimedOnDogecoin`. The same goes for untagged payments into the reserve (`unclaimedOnDogecoinVM`).
- **Consensus pins the reserve.** Block builders cannot redirect the reserve coinbase or mint more than it (`ErrBadPegReserve`, `ErrBadCoinbaseValue`).

## Trust model and limits

This is a federated peg: **the signers can move the locked DOGE.** m of the n signers could release reserve coins with no deposit behind them, or spend DOGE locked on Dogecoin. The audit would detect either, but it cannot prevent them. This is the same trust model as other federated pegs; making it trustless needs a Dogecoin light client inside DogecoinVM (Scrypt and AuxPoW header validation) and is not built.

The current bridge is a single process holding all signer keys, which suits development and a testnet only. Production needs:
- Each signer running its own process with its own key (ideally an HSM), co-signing releases and payments that it has verified independently.
- A deposit confirmation depth sized to Dogecoin reorg risk (tens of blocks, not 6).
- A peg-out delay and rate limits, so a compromised signer set is caught by the audit before everything moves.
- Monitoring and alerting on the audit.

Other limits:
- Deposits and peg-outs pay P2PKH or P2SH destinations only.
- Releases are sent one at a time, waiting for each to be accepted, because each spends the previous release's reserve change.
- `dogevm`'s Dogecoin side uses Dogecoin Core's wallet RPC: the peg address is imported watch-only, and `-txindex=1` is required.
