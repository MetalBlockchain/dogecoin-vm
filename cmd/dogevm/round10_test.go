package main

import (
	"path/filepath"
	"testing"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
	"github.com/stretchr/testify/require"
)

// droppedInCutover sets up signer 0 of a cosign harness with dropped, a
// transaction paying the peg address that the wallet saw and then lost,
// recorded in the signer's cutover. It returns the harness, the stubbed
// Dogecoin Core, a function that restarts the node (a fresh dogeChain on
// the same unsettled file) and one that returns the coins still owed.
func droppedInCutover(t *testing.T, h *cosignHarness, dropped *wire.MsgTx, extra map[string]string) (core *stubDogecoinCore, url, path string, restart func(), left func() []string) {
	require := require.New(t)
	signer := h.signers[0]
	s, err := signer.b.load()
	require.NoError(err)
	id := dropped.TxHash().String()
	bodies := map[string]string{id: encodeTx(dropped)}
	for k, v := range extra {
		bodies[k] = v
	}
	core = &stubDogecoinCore{listed: map[string]int64{id: 0}, bodies: bodies, blocks: map[string]stubBlock{}}
	srv := core.serve(t)
	path = filepath.Join(t.TempDir(), deadHistoryFile)
	url = srv.URL
	restart = func() {
		signer.b.doge = coreConflicts{fakeChain: h.doge,
			core: &dogeChain{rpc: newRPCClient(srv.URL, "", ""), dead: &deadHistory{path: path, depth: s.pegLine.rootDepth}}}
	}
	restart()
	_, err = signer.b.load() // listed, unconfirmed: kept in the file
	require.NoError(err)
	delete(core.listed, id) // then the wallet loses it
	h.adoptOne(signer)
	left = func() []string {
		s, err := signer.b.load()
		require.NoError(err)
		_, err = signer.advanceCutover(s)
		require.NoError(err)
		return signer.cutoverLeft(s)[chainDogecoin]
	}
	return core, url, path, restart, left
}

// TestOutputSpendProofIsRefused (Round 10, Z2; Codex r10 #2): a proof that
// one of a dropped transaction's OWN outputs is spent would say it was mined
// after all, so the customer claim it carried belongs in the books; Round 9
// cleared the coin from such a proof and lost that claim. Only a spend of
// one of its inputs (a conflict) is accepted now, and the output-spend case
// fails closed: the coin stays owed until the transaction is rescanned or
// imported.
func TestOutputSpendProofIsRefused(t *testing.T) {
	require := require.New(t)
	h0 := newCosignHarness(t)
	_, _, _, _ = h0.inUse()
	dropped := wire.NewMsgTx(1)
	dropped.AddTxIn(wire.NewTxIn(h0.coin(), nil, nil))
	dropped.AddTxOut(wire.NewTxOut(2*doge, h0.b.signers.pkScript()))
	id := dropped.TxHash().String()
	op := wire.OutPoint{Hash: dropped.TxHash(), Index: 0}.String()
	spend := wire.NewMsgTx(1)
	spend.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: dropped.TxHash()}, nil, nil))
	spend.AddTxOut(wire.NewTxOut(doge, h0.user(3).pkScript()))
	sid := spend.TxHash().String()

	core, url, path, _, left := droppedInCutover(t, h0, dropped, nil)
	require.Contains(left(), op)
	core.blocks["blk"] = stubBlock{confirmations: 40, tx: []string{sid}}

	node := &dogeChain{rpc: newRPCClient(url, "", "")}
	_, err := node.proofDepth(dropped, conflictProof{Spender: encodeTx(spend), Block: "blk"})
	require.ErrorContains(err, "doesn't spend any of the dropped transaction's inputs")

	// Recorded anyway (a hand-edited file): it still clears nothing.
	dead := deadHistory{path: path, depth: 20}
	require.NoError(dead.addProof(id, conflictProof{Spender: encodeTx(spend), Block: "blk"}))
	require.Contains(left(), op, "fails closed: still owed")
}

// TestProofsAccumulateAndSurviveARestart (Round 10, Z3; Codex r10 #3): a
// dropped transaction with two outputs to the peg address, both recorded.
// Proofs are kept side by side: a bad one recorded after a good one doesn't
// replace it, one conflict clears both outputs (the transaction can never
// be mined), and that holds after a restart.
func TestProofsAccumulateAndSurviveARestart(t *testing.T) {
	require := require.New(t)
	h0 := newCosignHarness(t)
	_, _, _, _ = h0.inUse()
	dropped := wire.NewMsgTx(1)
	dropped.AddTxIn(wire.NewTxIn(h0.coin(), nil, nil))
	dropped.AddTxOut(wire.NewTxOut(2*doge, h0.b.signers.pkScript()))
	dropped.AddTxOut(wire.NewTxOut(3*doge, h0.b.signers.pkScript()))
	id := dropped.TxHash().String()
	op0 := wire.OutPoint{Hash: dropped.TxHash(), Index: 0}.String()
	op1 := wire.OutPoint{Hash: dropped.TxHash(), Index: 1}.String()
	winner := dropped.Copy()
	winner.TxOut = []*wire.TxOut{wire.NewTxOut(4*doge, h0.user(9).pkScript())}
	wid := winner.TxHash().String()
	unrelated := wire.NewMsgTx(1)
	unrelated.AddTxIn(wire.NewTxIn(h0.coin(), nil, nil))
	unrelated.AddTxOut(wire.NewTxOut(doge, h0.user(8).pkScript()))

	core, _, path, restart, left := droppedInCutover(t, h0, dropped, nil)
	require.ElementsMatch([]string{op0, op1}, onlyOf(left(), op0, op1))
	st, err := h0.signers[0].b.load()
	require.NoError(err)
	depth := st.pegLine.rootDepth
	core.blocks["good"] = stubBlock{confirmations: depth, tx: []string{wid}}
	core.blocks["bad"] = stubBlock{confirmations: depth, tx: []string{unrelated.TxHash().String()}}

	dead := deadHistory{path: path, depth: depth}
	var records map[string]deadRecord
	require.NoError(dead.addProof(id, conflictProof{Spender: encodeTx(winner), Block: "good"}))
	require.NoError(dead.addProof(id, conflictProof{Spender: encodeTx(unrelated), Block: "bad"}))
	records, err = dead.read()
	require.NoError(err)
	require.Len(records[id].proofs(), 2, "the second proof is kept beside the first, not instead of it")

	core.blocks["good"] = stubBlock{confirmations: depth - 1, tx: []string{wid}}
	require.ElementsMatch([]string{op0, op1}, onlyOf(left(), op0, op1), "one short of rootDepth: both still owed")
	core.blocks["good"] = stubBlock{confirmations: depth, tx: []string{wid}}
	require.Empty(onlyOf(left(), op0, op1), "one conflict clears both outputs")
	restart()
	require.Empty(onlyOf(left(), op0, op1), "still cleared after a restart")
}

// onlyOf is the members of list among want.
func onlyOf(list []string, want ...string) []string {
	in := map[string]bool{}
	for _, w := range want {
		in[w] = true
	}
	var out []string
	for _, x := range list {
		if in[x] {
			out = append(out, x)
		}
	}
	return out
}
