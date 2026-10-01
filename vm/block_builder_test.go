// Copyright (C) 2024-2025, Metallicus, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package vm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestBuilderKeepsAnEarlySignal: a signal sent before anyone waits is not
// lost; the next wait returns at once.
func TestBuilderKeepsAnEarlySignal(t *testing.T) {
	vm := setupVM(t)
	b := vm.blockBuilder
	b.signalCanBuild()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, b.waitForNeedToBuild(ctx))
}

// TestBuilderWaitEndsWithItsContext: a wait with nothing to build returns
// promptly when its context ends, and leaves nothing behind.
func TestBuilderWaitEndsWithItsContext(t *testing.T) {
	vm := setupVM(t)
	b := vm.blockBuilder
	// Drain whatever start-up left, so there is nothing to build.
	for b.needToBuild() {
		buildBlock(t, vm)
	}
	b.clearPendingSignal()
	select {
	case <-b.wake:
	default:
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := b.waitForNeedToBuild(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second)
}

// TestBrokenVMBuildsAndVerifiesNothing: after an accept failed part-way,
// the VM refuses to build or verify until it restarts from disk.
func TestBrokenVMBuildsAndVerifiesNothing(t *testing.T) {
	vm := setupVM(t)
	blk := buildBlock(t, vm)
	vm.broken.Store(true)
	_, err := vm.BuildBlock(context.Background())
	require.ErrorIs(t, err, errBroken)
	require.ErrorIs(t, blk.Verify(context.Background()), errBroken)
	require.ErrorIs(t, blk.Accept(context.Background()), errBroken)
}
