// Copyright (c) 2013-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package btcd

import (
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// activeNetParams is a pointer to the parameters specific to the
// currently active DogecoinVM network.
var activeNetParams = &dogecoinVMTestNetParams

// params is used to group parameters for various networks such as the main
// network and test networks.
type params struct {
	*chaincfg.Params
	rpcPort string
}

const (
	// Network magics for DogecoinVM. These are deliberately different from
	// Dogecoin Core's (mainnet c0c0c0c0, testnet fcc1b7dc) so that a
	// DogecoinVM node can never be mistaken for a Dogecoin PoW peer.
	dogecoinVMMainNet wire.BitcoinNet = 0xd06ec0c0
	dogecoinVMTestNet wire.BitcoinNet = 0xd06eb7dc
)

var (
	bigOne                = big.NewInt(1)
	dogecoinVMPowLimit    = new(big.Int).Sub(new(big.Int).Lsh(bigOne, 255), bigOne)
	dogecoinVMGenesisTime = time.Unix(1790121600, 0) // 2026-09-23T00:00:00Z
)

// newGenesisBlock returns a DogecoinVM genesis block. The coinbase pays
// nothing to a provably unspendable script: there is no premine, and DOGE
// only enters the DogecoinVM ledger by being locked on Dogecoin mainnet.
func newGenesisBlock(message string) *wire.MsgBlock {
	pkScript, err := txscript.NullDataScript(nil)
	if err != nil {
		panic(err)
	}

	coinbase := &wire.MsgTx{
		Version: 1,
		TxIn: []*wire.TxIn{{
			PreviousOutPoint: wire.OutPoint{Index: wire.MaxPrevOutIndex},
			SignatureScript:  []byte(message),
			Sequence:         wire.MaxTxInSequenceNum,
		}},
		TxOut: []*wire.TxOut{{Value: 0, PkScript: pkScript}},
	}

	return &wire.MsgBlock{
		Header: wire.BlockHeader{
			Version:    1,
			MerkleRoot: coinbase.TxHash(),
			Timestamp:  dogecoinVMGenesisTime,
			Bits:       0x1d00ffff,
		},
		Transactions: []*wire.MsgTx{coinbase},
	}
}

var (
	dogecoinVMMainNetGenesisBlock = newGenesisBlock("DogecoinVM genesis - mainnet")
	dogecoinVMMainNetGenesisHash  = dogecoinVMMainNetGenesisBlock.BlockHash()

	dogecoinVMTestNetGenesisBlock = newGenesisBlock("DogecoinVM genesis - testnet")
	dogecoinVMTestNetGenesisHash  = dogecoinVMTestNetGenesisBlock.BlockHash()
)

// newDogecoinVMParams returns the consensus parameters shared by every
// DogecoinVM network. Blocks are ordered by Snowman, not proof of work, so the
// difficulty fields only need to be self-consistent; the PoW check itself is
// disabled in blockchain.checkProofOfWork.
func newDogecoinVMParams() chaincfg.Params {
	return chaincfg.Params{
		DNSSeeds: []chaincfg.DNSSeed{}, // NOTE: There must NOT be any seeds.

		// Chain parameters
		PowLimit:                 dogecoinVMPowLimit,
		PowLimitBits:             0x1d00ffff,
		BIP0034Height:            0, // Always active
		BIP0065Height:            0, // Always active
		BIP0066Height:            0, // Always active
		CoinbaseMaturity:         0,
		SubsidyReductionInterval: 210000,
		TargetTimespan:           time.Hour * 24 * 14, // 14 days
		TargetTimePerBlock:       time.Minute * 10,    // 10 minutes
		RetargetAdjustmentFactor: 4,                   // 25% less, 400% more
		ReduceMinDifficulty:      true,
		MinDiffReductionTime:     time.Minute * 20, // TargetTimePerBlock * 2
		GenerateSupported:        true,

		// DogecoinVM never mints DOGE through the coinbase. The PoW chain
		// keeps issuing 10,000 DOGE per block; mirroring that here would
		// double real issuance and unback the peg.
		NoBlockSubsidy: true,

		// Checkpoints ordered from oldest to newest.
		Checkpoints: nil,

		// Consensus rule change deployments.
		//
		// The miner confirmation window is defined as:
		//   target proof of work timespan / target proof of work spacing
		RuleChangeActivationThreshold: 75, // 75% of MinerConfirmationWindow
		MinerConfirmationWindow:       100,
		Deployments: [chaincfg.DefinedDeployments]chaincfg.ConsensusDeployment{
			chaincfg.DeploymentTestDummy: {
				BitNumber: 28,
				DeploymentStarter: chaincfg.NewMedianTimeDeploymentStarter(
					time.Time{}, // Always available for vote
				),
				DeploymentEnder: chaincfg.NewMedianTimeDeploymentEnder(
					time.Time{}, // Never expires
				),
			},
			chaincfg.DeploymentTestDummyMinActivation: {
				BitNumber:                 22,
				CustomActivationThreshold: 50,  // Only needs 50% hash rate.
				MinActivationHeight:       600, // Can only activate after height 600.
				DeploymentStarter: chaincfg.NewMedianTimeDeploymentStarter(
					time.Time{}, // Always available for vote
				),
				DeploymentEnder: chaincfg.NewMedianTimeDeploymentEnder(
					time.Time{}, // Never expires
				),
			},
			chaincfg.DeploymentCSV: {
				BitNumber: 0,
				DeploymentStarter: chaincfg.NewMedianTimeDeploymentStarter(
					time.Time{}, // Always available for vote
				),
				DeploymentEnder: chaincfg.NewMedianTimeDeploymentEnder(
					time.Time{}, // Never expires
				),
			},
			// Dogecoin has neither SegWit nor Taproot. A deployment that
			// has started gets signalled by every block template and
			// would lock in within a few hundred blocks, so these never
			// start.
			chaincfg.DeploymentSegwit:  neverStartedDeployment(1),
			chaincfg.DeploymentTaproot: neverStartedDeployment(2),
			chaincfg.DeploymentTestDummyAlwaysActive: {
				BitNumber: 29,
				DeploymentStarter: chaincfg.NewMedianTimeDeploymentStarter(
					time.Time{}, // Always available for vote
				),
				DeploymentEnder: chaincfg.NewMedianTimeDeploymentEnder(
					time.Time{}, // Never expires
				),
				AlwaysActiveHeight: 1,
			},
		},

		// Mempool parameters. Standardness carries Dogecoin's dust rules
		// and keeps witness outputs out of the mempool, so it is enforced
		// on every network.
		RelayNonStdTxs: false,
	}
}

// neverStartedDeployment returns a deployment that never starts, so it can
// never be signalled, locked in or activated.
func neverStartedDeployment(bit uint8) chaincfg.ConsensusDeployment {
	return chaincfg.ConsensusDeployment{
		BitNumber: bit,
		DeploymentStarter: chaincfg.NewMedianTimeDeploymentStarter(
			time.Date(9999, time.December, 31, 0, 0, 0, 0, time.UTC),
		),
		DeploymentEnder: chaincfg.NewMedianTimeDeploymentEnder(
			time.Time{}, // Never expires.
		),
	}
}

// DogecoinVMMainNetParams defines the network parameters for DogecoinVM
// mainnet. Address and key encodings are identical to Dogecoin mainnet, so a
// D... address is the same address on both ledgers.
var DogecoinVMMainNetParams = func() chaincfg.Params {
	p := newDogecoinVMParams()
	p.Name = "dogecoinvm"
	p.Net = dogecoinVMMainNet
	// DogecoinVM does not listen for P2P connections; ports sit next to
	// Dogecoin Core's (22556/22555) without colliding with them.
	p.DefaultPort = "22566"
	p.GenesisBlock = dogecoinVMMainNetGenesisBlock
	p.GenesisHash = &dogecoinVMMainNetGenesisHash

	// Dogecoin has no bech32 addresses. With an empty HRP no string
	// decodes as a segwit address.
	p.Bech32HRPSegwit = ""

	// Address encoding magics (Dogecoin Core chainparams.cpp, CMainParams)
	p.PubKeyHashAddrID = 30 // starts with D
	p.ScriptHashAddrID = 22 // starts with 9 or A
	p.PrivateKeyID = 158    // starts with 6 (uncompressed) or Q (compressed)
	p.WitnessPubKeyHashAddrID = 0x00
	p.WitnessScriptHashAddrID = 0x00

	// BIP32 hierarchical deterministic extended key magics
	p.HDPrivateKeyID = [4]byte{0x02, 0xfa, 0xc3, 0x98} // starts with dgpv
	p.HDPublicKeyID = [4]byte{0x02, 0xfa, 0xca, 0xfd}  // starts with dgub

	// SLIP-0044 coin type for Dogecoin.
	p.HDCoinType = 3
	return p
}()

// DogecoinVMTestNetParams defines the network parameters for DogecoinVM
// testnet, which pegs against Dogecoin testnet and shares its address and key
// encodings.
var DogecoinVMTestNetParams = func() chaincfg.Params {
	p := newDogecoinVMParams()
	p.Name = "dogecoinvmtestnet"
	p.Net = dogecoinVMTestNet
	// DogecoinVM does not listen for P2P connections; ports sit next to
	// Dogecoin Core's testnet (44556/44555) without colliding with them.
	p.DefaultPort = "44566"
	p.GenesisBlock = dogecoinVMTestNetGenesisBlock
	p.GenesisHash = &dogecoinVMTestNetGenesisHash

	// Dogecoin has no bech32 addresses. With an empty HRP no string
	// decodes as a segwit address.
	p.Bech32HRPSegwit = ""

	// Address encoding magics (Dogecoin Core chainparams.cpp, CTestNetParams)
	p.PubKeyHashAddrID = 113 // starts with n
	p.ScriptHashAddrID = 196 // starts with 2
	p.PrivateKeyID = 241     // starts with 9 (uncompressed) or c (compressed)
	p.WitnessPubKeyHashAddrID = 0x00
	p.WitnessScriptHashAddrID = 0x00

	// BIP32 hierarchical deterministic extended key magics
	p.HDPrivateKeyID = [4]byte{0x04, 0x35, 0x83, 0x94} // starts with tprv
	p.HDPublicKeyID = [4]byte{0x04, 0x35, 0x87, 0xcf}  // starts with tpub

	// SLIP-0044 coin type for all testnets.
	p.HDCoinType = 1
	return p
}()

// PegReserveAmountPerBlock is what each peg reserve coinbase pays into the
// reserve: 9 billion DOGE, leaving room under the 10 billion DOGE
// per-transaction limit for the block's fees.
const PegReserveAmountPerBlock = 9_000_000_000 * 1e8

// withPegReserve returns a copy of p whose chain params lock blocks worth of
// PegReserveAmountPerBlock to address.
func withPegReserve(p *params, address string, blocks int32) (*params, error) {
	if address == "" || blocks < 1 {
		return nil, errors.New("pegReserveAddress and a positive " +
			"pegReserveBlocks must be set together")
	}
	addr, err := btcutil.DecodeAddress(address, p.Params)
	if err != nil {
		return nil, fmt.Errorf("invalid pegReserveAddress: %w", err)
	}
	if !addr.IsForNet(p.Params) {
		return nil, fmt.Errorf("pegReserveAddress %s is not for %s",
			address, p.Name)
	}
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return nil, err
	}

	chainParams := *p.Params
	chainParams.PegReserve = &chaincfg.PegReserve{
		PkScript: pkScript,
		Amount:   PegReserveAmountPerBlock,
		Blocks:   blocks,
	}
	return &params{Params: &chainParams, rpcPort: p.rpcPort}, nil
}

var dogecoinVMMainNetParams = params{
	Params:  &DogecoinVMMainNetParams,
	rpcPort: "22565",
}

var dogecoinVMTestNetParams = params{
	Params:  &DogecoinVMTestNetParams,
	rpcPort: "44565",
}

// netName returns the name used when referring to a DogecoinVM network, which
// is also the data and log directory name.
func netName(chainParams *params) string {
	return chainParams.Name
}

// Register the DogecoinVM networks so address and extended key lookups (for
// example dgpv -> dgub in hdkeychain.Neuter) recognise their encodings.
func init() {
	for _, p := range []*chaincfg.Params{&DogecoinVMMainNetParams, &DogecoinVMTestNetParams} {
		if err := chaincfg.Register(p); err != nil {
			panic(err)
		}
	}
}
