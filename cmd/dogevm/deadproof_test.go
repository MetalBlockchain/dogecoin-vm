package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// The stub's tip is height 100: a transaction listed with n confirmations is
// in block 101-n.

// TestDroppedTransactionClearsOnADeepWalletConflict (W8): a dropped
// transaction whose input the wallet sees spent in a block clears once that
// spend is rootDepth deep, at a depth read fresh each time (a reorganisation
// can make it shallower again), and stays cleared across a restart.
func TestDroppedTransactionClearsOnADeepWalletConflict(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	dropped := h.contribute(doge, 0)
	id := dropped.TxHash().String()
	winner := dropped.Copy()
	winner.TxOut[0].Value-- // same inputs, another transaction
	wid := winner.TxHash().String()
	core := &stubDogecoinCore{listed: map[string]int64{id: 0}, bodies: map[string]string{id: encodeTx(dropped), wid: encodeTx(winner)}}
	srv := core.serve(t)
	path := filepath.Join(t.TempDir(), deadHistoryFile)
	client := func() *dogeChain {
		return &dogeChain{rpc: newRPCClient(srv.URL, "", ""), dead: &deadHistory{path: path, depth: 20}}
	}
	c := client()
	_, _, err := c.history()
	require.NoError(err)

	delete(core.listed, id) // dropped; the winner is in a block 3 deep
	core.listed[wid] = 3
	_, entries, err := c.history()
	require.NoError(err)
	require.True(entries[id].conflicted)
	require.EqualValues(3, entries[id].conflictDepth)

	core.listed[wid] = 10
	_, entries, err = c.history()
	require.NoError(err)
	require.EqualValues(10, entries[id].conflictDepth)
	core.listed[wid] = 4 // a reorganisation: shallower again, and so is the evidence
	_, entries, err = c.history()
	require.NoError(err)
	require.EqualValues(4, entries[id].conflictDepth, "depth is read fresh, not remembered")

	core.listed[wid] = 20
	_, entries, err = c.history()
	require.NoError(err)
	require.NotContains(entries, id, "settled: its spend is rootDepth deep")
	records, err := (&deadHistory{path: path}).read()
	require.NoError(err)
	require.NotContains(records, id)
	_, entries, err = client().history() // a restart
	require.NoError(err)
	require.NotContains(entries, id)
}

// TestDroppedTransactionClearsOnAProvenConflict (W8): when the winner pays
// someone else (the wallet never lists it), an operator records its bytes
// and block; the node checks them on every read. A spender of none of the
// dropped transaction's inputs, or a block that doesn't hold it, counts for
// nothing.
func TestDroppedTransactionClearsOnAProvenConflict(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	dropped := h.contribute(doge, 0)
	id := dropped.TxHash().String()
	winner := dropped.Copy()
	winner.TxOut[0].Value--
	wid := winner.TxHash().String()
	unrelated := h.contribute(doge, 0)
	core := &stubDogecoinCore{listed: map[string]int64{id: 0}, bodies: map[string]string{id: encodeTx(dropped)},
		blocks: map[string]stubBlock{"B": {confirmations: 3, tx: []string{wid}}, "E": {confirmations: 30, tx: []string{"00"}}}}
	srv := core.serve(t)
	path := filepath.Join(t.TempDir(), deadHistoryFile)
	c := &dogeChain{rpc: newRPCClient(srv.URL, "", ""), dead: &deadHistory{path: path, depth: 20}}
	_, _, err := c.history()
	require.NoError(err)
	delete(core.listed, id)

	// Bad proofs: a spender of something else, and a block without it.
	require.NoError(c.dead.setProof(id, conflictProof{Spender: encodeTx(unrelated), Block: "B"}))
	_, entries, err := c.history()
	require.NoError(err)
	require.Zero(entries[id].conflictDepth)
	require.NoError(c.dead.setProof(id, conflictProof{Spender: encodeTx(winner), Block: "E"}))
	_, entries, err = c.history()
	require.NoError(err)
	require.Zero(entries[id].conflictDepth)

	require.NoError(c.dead.setProof(id, conflictProof{Spender: encodeTx(winner), Block: "B"}))
	_, entries, err = c.history()
	require.NoError(err)
	require.EqualValues(3, entries[id].conflictDepth, "a good proof, not yet deep")
	core.blocks["B"] = stubBlock{confirmations: 0, tx: []string{wid}} // reorganised out
	_, entries, err = c.history()
	require.NoError(err)
	require.Zero(entries[id].conflictDepth)

	core.blocks["B"] = stubBlock{confirmations: 20, tx: []string{wid}}
	_, entries, err = c.history()
	require.NoError(err)
	require.NotContains(entries, id, "settled by the proof")
}
