package vm

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFreshNodeParsesGenesisFromPeer: a peer's reply to a joining node's
// request for blocks runs down to genesis, and the node parses every block
// in it. Genesis has no BIP34 height in its coinbase, so the node must
// recognise it as its own rather than fail to parse it (and then ask for the
// same blocks forever).
func TestFreshNodeParsesGenesisFromPeer(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	a := setupVM(t)
	b := setupVM(t)
	genesis, err := a.GetBlock(ctx, a.LastAcceptedID())
	require.NoError(err)
	require.Zero(genesis.Height())

	parsed, err := b.ParseBlock(ctx, genesis.Bytes())
	require.NoError(err, "a fresh node parses genesis bytes from a peer")
	require.Equal(genesis.ID(), parsed.ID())
	require.Zero(parsed.Height())

	// And the chain that follows.
	acceptBlocks(t, a, 2)
	for id := a.LastAcceptedID(); id != genesis.ID(); {
		blk, err := a.GetBlock(ctx, id)
		require.NoError(err)
		_, err = b.ParseBlock(ctx, blk.Bytes())
		require.NoError(err)
		id = blk.Parent()
	}
}
