# Genesis

> Inherited from upstream btcvm; DogecoinVM doesn't use this. Its genesis is built by `cmd/dogevm-l1` with the consensus peg reserve and no premine.

The upstream guide that was here described `cmd/genesis-generator`, which makes a Bitcoin genesis paying a block reward to one key. DogecoinVM has no premine and no block reward: the genesis blocks pay nothing, and DOGE enters only through the peg reserve, released against DOGE locked on Dogecoin.

- A local network: `scripts/devnet.sh start` creates the genesis and chain ([TESTING.md](TESTING.md)).
- Mainnet: `deploy/mainnet.sh` writes the genesis (`mainNet`, `pegReserveAddress`, `pegReserveBlocks`) and `cmd/dogevm-l1 create` launches the L1.
- The peg reserve and its rules: [BRIDGE.md](BRIDGE.md).
