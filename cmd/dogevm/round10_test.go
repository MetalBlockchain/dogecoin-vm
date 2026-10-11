package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
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

// TestInventoryPricesTheFencesTheCutoverSigns (Round 10, Z4; Codex r10 #4):
// the inventory builds the same fences the cutover does, in the same
// order (the tip, then oldest first), with each input's own redeem script
// and the real change carried forward. Here older personal-deposit coins
// (a longer redeem script) fall into the first batch by age, not where
// their outpoints would sort; the inventory's fees equal what the cutover
// actually paid.
func TestInventoryPricesTheFencesTheCutoverSigns(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	per := maxFenceInputs(chainDogecoin)
	for range per + 10 {
		h.contribute(doge, 100)
	}
	// One personal-deposit coin, older than every contribution (so the
	// cutover's age order puts it in the first, full batch) but with an
	// outpoint that sorts after all of them (so outpoint order would leave
	// it to the small second batch). Its longer redeem script takes the
	// full batch past a fee step.
	dest := h.user(40)
	_, err := h.b.registry.add(dest)
	require.NoError(err)
	for _, c := range h.signers {
		_, err := c.b.registry.add(dest)
		require.NoError(err)
	}
	var last chainhash.Hash
	for _, t := range h.doge.txs {
		if hash := t.tx.TxHash(); bytes.Compare(hash[:], last[:]) > 0 {
			last = hash
		}
	}
	for value := int64(2 * doge); ; value++ {
		tx := wire.NewMsgTx(1)
		tx.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
		tx.AddTxOut(wire.NewTxOut(value, p2shScript(h.b.signers.depositRedeemScript(dest))))
		if hash := tx.TxHash(); bytes.Compare(hash[:], last[:]) > 0 {
			h.doge.add(tx, 300)
			break
		}
	}
	s, err := h.b.load()
	require.NoError(err)
	inv, err := nodeInventory(h.b, s)
	require.NoError(err)
	ic := inv.Chains[chainDogecoin]
	require.Equal(2, ic.Batches)

	h.rebuildLogs()
	h.fenceUntilDone(80)
	var paid int64
	s, err = h.b.load()
	require.NoError(err)
	for _, t := range h.doge.txs {
		if _, ok := parseFence(t.tx); ok {
			fee := fenceFeeOf(s, chainDogecoin, t.tx)
			require.Positive(fee)
			paid += fee
		}
	}
	require.Equal(ic.FenceFees, formatDoge(paid), "the inventory priced exactly what the cutover paid")
}

// TestInventoryReportsAnInfeasibleFence (Round 10, Z4): a batch whose
// coins can't pay its fee and leave the next anchor is a problem the
// inventory names, not a number it adds up.
func TestInventoryReportsAnInfeasibleFence(t *testing.T) {
	require := require.New(t)
	h := newHarness(t) // the peg holds only the seed: one fence fee, nothing left
	s, err := h.b.load()
	require.NoError(err)
	inv, err := nodeInventory(h.b, s)
	require.NoError(err)
	require.False(inv.Ready)
	require.Contains(strings.Join(inv.Problems, "\n"), "not enough for the fence's")
}
