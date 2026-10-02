package vm

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/blockchain"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// The accept latch on real failures (round 4, T16), not by setting it:
// a block that breaks a consensus rule is refused before btcd changes
// anything, so the VM carries on; a failure in btcd's storage, or btcd
// taking an accepted block as anything but its new tip, latches it.

// TestRuleErrorOnAcceptDoesntLatch: Snowman accepting a block that breaks
// a rule (it was never verified here) fails with a rule error; btcd
// changed nothing, so the VM stays healthy and accepts a valid block next.
func TestRuleErrorOnAcceptDoesntLatch(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	vm := setupVM(t)
	blk := buildBlock(t, vm)
	bad := parse(t, vm, mutateBlock(t, blk, 1, 7, func(msg *wire.MsgBlock) {
		msg.Transactions[0].TxOut[0].Value++ // mints DOGE
	}))
	err := bad.Accept(ctx)
	var rule blockchain.RuleError
	require.True(errors.As(err, &rule), "got %v", err)
	require.False(vm.broken.Load())
	_, err = vm.HealthCheck(ctx)
	require.NoError(err)

	require.NoError(blk.Verify(ctx))
	require.NoError(blk.Accept(ctx))
	require.Equal(blk.ID(), tipID(vm))
}

// TestPersistFailureOnAcceptLatches: btcd took the block, but can't write
// it to disk (its data directory became read-only), so what is on disk and
// what the VM has accepted may differ: the VM latches, and builds,
// verifies and accepts nothing more, and its health check fails, until it
// restarts.
func TestPersistFailureOnAcceptLatches(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	vm := newTestVM(t, dir, nil)
	// No Shutdown: leveldb, after its write failed, waits for good on the
	// close's flush, which is why a latched node is restarted, not stopped
	// cleanly. The directory is made writable again so it can be removed.
	t.Cleanup(func() {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	blk := buildBlock(t, vm)
	require.NoError(blk.Verify(ctx))
	require.NoError(filepath.WalkDir(filepath.Join(dir, "data"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o400)
		if d.IsDir() {
			mode = 0o500
		}
		return os.Chmod(p, mode)
	}))
	err := blk.Accept(ctx)
	require.ErrorContains(err, "failed to persist")
	require.True(vm.broken.Load(), "latched by %v", err)
	_, err = vm.HealthCheck(ctx)
	require.ErrorIs(err, errBroken)
	_, err = vm.BuildBlock(ctx)
	require.ErrorIs(err, errBroken)
}

// TestAcceptedBlockNotTheTipLatches: btcd's tip moved without the VM (here,
// another block at the same height went straight into btcd), so the block
// Snowman accepts lands beside it, not as the tip: what btcd has and what
// the VM has accepted disagree, and the VM latches.
func TestAcceptedBlockNotTheTipLatches(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	vm := setupVM(t)
	a := buildBlock(t, vm)
	sibling := parse(t, vm, mutateBlock(t, a, 1, 99, nil)).(*BlockAdapter)
	isMain, isOrphan, err := vm.btcdAdapter.ProcessBlockNoPoW(sibling.btcBlock)
	require.NoError(err)
	require.True(isMain && !isOrphan)

	err = a.Accept(ctx)
	require.ErrorContains(err, "did not become the chain tip")
	require.True(vm.broken.Load())
	_, err = vm.HealthCheck(ctx)
	require.ErrorIs(err, errBroken)
}

// TestDatabaseFailureOnAcceptLatches: btcd's database fails while it
// connects the block (here, closed under it): not a rule error, and btcd
// may be half done, so the VM latches.
func TestDatabaseFailureOnAcceptLatches(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	vm := setupVM(t)
	blk := buildBlock(t, vm)
	require.NoError(blk.Verify(ctx))
	require.NoError(vm.btcdAdapter.Database().Close())
	err := blk.Accept(ctx)
	require.ErrorContains(err, "failed to connect accepted block")
	var rule blockchain.RuleError
	require.False(errors.As(err, &rule), "a database failure, not a rule: %v", err)
	require.True(vm.broken.Load())
	_, err = vm.HealthCheck(ctx)
	require.ErrorIs(err, errBroken)
	require.ErrorIs(blk.Verify(ctx), errBroken)
}
