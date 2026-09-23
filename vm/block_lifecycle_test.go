package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/MetalBlockchain/metalgo/database"
	"github.com/MetalBlockchain/metalgo/database/memdb"
	"github.com/MetalBlockchain/metalgo/ids"
	"github.com/MetalBlockchain/metalgo/snow/consensus/snowman"
	"github.com/MetalBlockchain/metalgo/snow/snowtest"
	"github.com/stretchr/testify/require"

	btcd "github.com/MetalBlockchain/dogecoin-vm/btcd"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/blockchain"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// TestMain hides the go test flags from btcd, whose LoadConfig parses
// os.Args.
func TestMain(m *testing.M) {
	flag.Parse()
	os.Args = os.Args[:1]
	os.Exit(m.Run())
}

// newTestVM starts a VM on DogecoinVM testnet with its btcd data in dir.
// extra is merged into the btcd config in the genesis.
func newTestVM(t *testing.T, dir string, extra map[string]any) *VM {
	t.Helper()
	vm, err := startTestVM(t, dir, extra)
	require.NoError(t, err)
	return vm
}

// startTestVM is newTestVM, returning Initialize's error.
func startTestVM(t *testing.T, dir string, extra map[string]any) (*VM, error) {
	t.Helper()
	require := require.New(t)

	miningAddr, err := btcutil.NewAddressPubKeyHash(bytes.Repeat([]byte{0x01}, 20), &btcd.DogecoinVMTestNetParams)
	require.NoError(err)

	// btcd copies a sample config next to the binary if none exists.
	configFile := filepath.Join(dir, "btcd.conf")
	require.NoError(os.WriteFile(configFile, nil, 0o600))

	config := map[string]any{
		"configFile":  configFile,
		"testNet":     true,
		"dataDir":     filepath.Join(dir, "data"),
		"logDir":      filepath.Join(dir, "logs"),
		"miningAddrs": []string{miningAddr.EncodeAddress()},
		"disableRPC":  true,
	}
	for k, v := range extra {
		config[k] = v
	}
	genesis, err := json.Marshal(map[string]any{"config": config})
	require.NoError(err)

	vm := &VM{}
	snowCtx := snowtest.Context(t, ids.GenerateTestID())
	err = vm.Initialize(context.Background(), snowCtx, memdb.New(), genesis, nil, nil, nil, nil)
	return vm, err
}

func setupVM(t *testing.T) *VM {
	t.Helper()
	return setupVMWithConfig(t, nil)
}

func setupVMWithConfig(t *testing.T, extra map[string]any) *VM {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // btcd derives default paths from $HOME
	vm := newTestVM(t, t.TempDir(), extra)
	t.Cleanup(func() { _ = vm.Shutdown(context.Background()) })
	return vm
}

func buildBlock(t *testing.T, vm *VM) *BlockAdapter {
	t.Helper()
	blk, err := vm.BuildBlock(context.Background())
	require.NoError(t, err)
	return blk.(*BlockAdapter)
}

// tipID returns the ID of btcd's chain tip.
func tipID(vm *VM) ids.ID {
	return hashToID(&vm.chain.BestSnapshot().Hash)
}

// mutateBlock returns the bytes of a copy of blk after applying f, with the
// coinbase rebuilt for coinbaseHeight and the merkle root recomputed.
func mutateBlock(t *testing.T, blk *BlockAdapter, coinbaseHeight int64, extraNonce int64, f func(*wire.MsgBlock)) []byte {
	t.Helper()
	require := require.New(t)

	var msg wire.MsgBlock
	require.NoError(msg.Deserialize(bytes.NewReader(blk.Bytes())))

	sigScript, err := txscript.NewScriptBuilder().AddInt64(coinbaseHeight).AddInt64(extraNonce).Script()
	require.NoError(err)
	msg.Transactions[0].TxIn[0].SignatureScript = sigScript
	if f != nil {
		f(&msg)
	}

	block := btcutil.NewBlock(&msg)
	msg.Header.MerkleRoot = blockchain.CalcMerkleRoot(block.Transactions(), false)

	var buf bytes.Buffer
	require.NoError(msg.Serialize(&buf))
	return buf.Bytes()
}

func parse(t *testing.T, vm *VM, b []byte) snowman.Block {
	t.Helper()
	blk, err := vm.ParseBlock(context.Background(), b)
	require.NoError(t, err)
	return blk
}

func TestBuildVerifyAccept(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	vm := setupVM(t)

	genesisID := tipID(vm)
	lastAccepted, err := vm.LastAccepted(ctx)
	require.NoError(err)
	require.Equal(genesisID, lastAccepted)

	blk := buildBlock(t, vm)
	require.Equal(genesisID, blk.Parent())
	require.Equal(uint64(1), blk.Height())

	// Building does not touch btcd, and an unverified block is unknown.
	require.Equal(genesisID, tipID(vm))
	_, err = vm.GetBlock(ctx, blk.ID())
	require.ErrorIs(err, database.ErrNotFound)

	// Verifying does not touch btcd either.
	require.NoError(blk.Verify(ctx))
	require.Equal(genesisID, tipID(vm))
	got, err := vm.GetBlock(ctx, blk.ID())
	require.NoError(err)
	require.Equal(blk, got)

	// No new block is built while one is being decided.
	_, err = vm.BuildBlock(ctx)
	require.ErrorIs(err, errBlockProcessing)

	// Accept is the only step that connects the block.
	require.NoError(blk.Accept(ctx))
	require.Equal(blk.ID(), tipID(vm))
	lastAccepted, err = vm.LastAccepted(ctx)
	require.NoError(err)
	require.Equal(blk.ID(), lastAccepted)

	got, err = vm.GetBlock(ctx, blk.ID())
	require.NoError(err)
	require.Equal(uint64(1), got.Height())
	require.Equal(blk.Bytes(), got.Bytes())

	heightID, err := vm.GetBlockIDAtHeight(ctx, 1)
	require.NoError(err)
	require.Equal(blk.ID(), heightID)

	// Building resumes on top of the accepted block.
	next := buildBlock(t, vm)
	require.Equal(blk.ID(), next.Parent())
	require.Equal(uint64(2), next.Height())
}

func TestRejectLeavesChainUntouched(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	vm := setupVM(t)
	genesisID := tipID(vm)

	blk := buildBlock(t, vm)
	require.NoError(blk.Verify(ctx))
	require.NoError(blk.Reject(ctx))

	require.Equal(genesisID, tipID(vm))
	lastAccepted, err := vm.LastAccepted(ctx)
	require.NoError(err)
	require.Equal(genesisID, lastAccepted)
	_, err = vm.GetBlock(ctx, blk.ID())
	require.ErrorIs(err, database.ErrNotFound)

	has, err := vm.chain.HaveBlock(idToHash(blk.ID()))
	require.NoError(err)
	require.False(has)

	// The builder is unblocked after the rejection.
	next := buildBlock(t, vm)
	require.Equal(genesisID, next.Parent())
}

func TestCompetingSiblings(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	vm := setupVM(t)

	a := buildBlock(t, vm)
	sibling := parse(t, vm, mutateBlock(t, a, 1, 99, nil))
	require.NotEqual(a.ID(), sibling.ID())
	require.Equal(a.Parent(), sibling.Parent())

	// Parsing a block does not store it.
	has, err := vm.chain.HaveBlock(idToHash(sibling.ID()))
	require.NoError(err)
	require.False(has)

	require.NoError(a.Verify(ctx))
	require.NoError(sibling.Verify(ctx))

	// A child of a block that is still being decided cannot be verified.
	child := parse(t, vm, mutateBlock(t, a, 2, 0, func(msg *wire.MsgBlock) {
		msg.Header.PrevBlock = *a.btcBlock.Hash()
	}))
	require.ErrorIs(child.Verify(ctx), errParentNotAccepted)

	// Snowman accepts one sibling and rejects the other.
	require.NoError(a.Accept(ctx))
	require.NoError(sibling.Reject(ctx))
	require.Equal(a.ID(), tipID(vm))

	// The rejected sibling can no longer be verified or accepted.
	require.ErrorIs(sibling.Verify(ctx), errParentNotAccepted)
	require.ErrorIs(sibling.Accept(ctx), errParentNotAccepted)
	require.Equal(a.ID(), tipID(vm))
}

func TestVerifyRejectsInvalidBlocks(t *testing.T) {
	ctx := context.Background()
	vm := setupVM(t)
	blk := buildBlock(t, vm)

	tests := []struct {
		name    string
		bytes   []byte
		wantErr error
	}{
		{
			name:    "coinbase height does not follow parent",
			bytes:   mutateBlock(t, blk, 5, 0, nil),
			wantErr: errWrongHeight,
		},
		{
			name: "coinbase mints DOGE",
			bytes: mutateBlock(t, blk, 1, 0, func(msg *wire.MsgBlock) {
				msg.Transactions[0].TxOut[0].Value = 1
			}),
			wantErr: blockchain.RuleError{ErrorCode: blockchain.ErrBadCoinbaseValue},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			require := require.New(t)
			genesisID := tipID(vm)

			b := parse(t, vm, test.bytes)
			err := b.Verify(ctx)
			require.Error(err)
			var ruleErr blockchain.RuleError
			if want, ok := test.wantErr.(blockchain.RuleError); ok {
				require.True(errors.As(err, &ruleErr), "got %v", err)
				require.Equal(want.ErrorCode, ruleErr.ErrorCode)
			} else {
				require.ErrorIs(err, test.wantErr)
			}

			require.Equal(genesisID, tipID(vm))
			_, err = vm.GetBlock(ctx, b.ID())
			require.ErrorIs(err, database.ErrNotFound)
		})
	}
}

func TestParseRejectsNonCanonicalBytes(t *testing.T) {
	vm := setupVM(t)
	blk := buildBlock(t, vm)

	_, err := vm.ParseBlock(context.Background(), append(bytes.Clone(blk.Bytes()), 0x00))
	require.ErrorIs(t, err, errNonCanonicalBlock)
}

func TestParseReturnsKnownBlock(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	vm := setupVM(t)

	blk := buildBlock(t, vm)
	require.NoError(blk.Verify(ctx))
	require.Same(blk, parse(t, vm, blk.Bytes()))

	require.NoError(blk.Accept(ctx))
	accepted := parse(t, vm, blk.Bytes())
	require.Equal(blk.ID(), accepted.ID())
	require.Equal(uint64(1), accepted.Height())
}

func TestLastAcceptedSurvivesRestart(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()

	vm := newTestVM(t, dir, nil)
	blk := buildBlock(t, vm)
	require.NoError(blk.Verify(ctx))
	require.NoError(blk.Accept(ctx))

	// A verified but unaccepted block must not survive a restart.
	pending := buildBlock(t, vm)
	require.NoError(pending.Verify(ctx))
	require.NoError(vm.Shutdown(ctx))

	vm = newTestVM(t, dir, nil)
	defer func() { _ = vm.Shutdown(ctx) }()

	lastAccepted, err := vm.LastAccepted(ctx)
	require.NoError(err)
	require.Equal(blk.ID(), lastAccepted)
	_, err = vm.GetBlock(ctx, pending.ID())
	require.ErrorIs(err, database.ErrNotFound)
}

// acceptBlocks builds, verifies and accepts n blocks.
func acceptBlocks(t *testing.T, vm *VM, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		blk := buildBlock(t, vm)
		require.NoError(t, blk.Verify(ctx))
		require.NoError(t, blk.Accept(ctx))
	}
}

// TestSegwitAndTaprootNeverActivate checks that DogecoinVM never activates
// SegWit or Taproot. Under btcvm's parameters both locked in within a few
// hundred blocks, because every block template signalled for them.
func TestSegwitAndTaprootNeverActivate(t *testing.T) {
	require := require.New(t)
	vm := setupVM(t)

	// Four miner confirmation windows: enough to start, lock in and
	// activate a deployment that every block signals for.
	acceptBlocks(t, vm, 4*int(vm.config.ChainParams.MinerConfirmationWindow))

	for _, deployment := range []uint32{chaincfg.DeploymentSegwit, chaincfg.DeploymentTaproot} {
		active, err := vm.chain.IsDeploymentActive(deployment)
		require.NoError(err)
		require.False(active, "deployment %d is active", deployment)
	}
}

// TestVerifyRejectsWitnessData checks that a block carrying witness data is
// invalid while SegWit is inactive.
func TestVerifyRejectsWitnessData(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	vm := setupVM(t)
	blk := buildBlock(t, vm)

	b := parse(t, vm, mutateBlock(t, blk, 1, 0, func(msg *wire.MsgBlock) {
		msg.Transactions[0].TxIn[0].Witness = wire.TxWitness{make([]byte, 32)}
	}))
	err := b.Verify(ctx)
	var ruleErr blockchain.RuleError
	require.True(errors.As(err, &ruleErr), "got %v", err)
	require.Equal(blockchain.ErrUnexpectedWitness, ruleErr.ErrorCode)
}
