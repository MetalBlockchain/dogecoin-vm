package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// Round 8 (X1–X3): what the round-7 reviews found still open.

// coreConflicts is a fake Dogecoin chain whose conflicted transactions come
// from a real dogeChain over a stub of Dogecoin Core: the history reader,
// the unsettled file and the conflict evidence, as a node runs them.
type coreConflicts struct {
	*fakeChain
	core *dogeChain
}

func (c coreConflicts) conflictedFor(a []btcutil.Address) ([]chainTx, error) {
	return c.core.conflictedFor(a)
}

// TestDroppedCoinClearsInACutoverAndStaysCleared (X1, both r8 reviews): a
// signer's cutover records the output of a transaction the wallet dropped.
// Through load, cutoverLeft and advanceCutover: owed while the spend that
// conflicts with it is shallower than rootDepth (3, then rootDepth-1),
// cleared once it is rootDepth deep, still cleared after a restart, and owed
// again if a reorganisation makes that spend shallow again. Round 7 deleted
// the dropped transaction's record the moment its conflict was deep, which
// took away the only evidence that cleared its output: the coin stayed in
// cutoverLeft for good.
func TestDroppedCoinClearsInACutoverAndStaysCleared(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	_, _, _, _ = h.inUse()
	signer := h.signers[0]
	s, err := signer.b.load()
	require.NoError(err)
	depth := s.pegLine.rootDepth

	// A contribution to the peg address the wallet saw, then dropped: a
	// payment double-spending its input (to someone else) won the race.
	dropped := wire.NewMsgTx(1)
	dropped.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
	dropped.AddTxOut(wire.NewTxOut(2*doge, h.b.signers.pkScript()))
	dropped.AddTxOut(nullData(tagContrib))
	id := dropped.TxHash().String()
	op := wire.OutPoint{Hash: dropped.TxHash(), Index: 0}.String()
	winner := dropped.Copy()
	winner.TxOut[0].PkScript = h.user(9).pkScript()
	wid := winner.TxHash().String()
	core := &stubDogecoinCore{listed: map[string]int64{id: 0}, bodies: map[string]string{id: encodeTx(dropped), wid: encodeTx(winner)}}
	srv := core.serve(t)
	path := filepath.Join(t.TempDir(), deadHistoryFile)
	node := func() *dogeChain {
		return &dogeChain{rpc: newRPCClient(srv.URL, "", ""), dead: &deadHistory{path: path, depth: depth}}
	}
	signer.b.doge = coreConflicts{fakeChain: h.doge, core: node()}
	_, err = signer.b.load() // the wallet lists it, unconfirmed: kept
	require.NoError(err)

	delete(core.listed, id)
	core.listed[wid] = 3
	h.adoptOne(signer)
	require.Contains(signer.log.Cutover[chainDogecoin], op, "the dropped transaction's output is recorded")

	left := func() []string {
		s, err := signer.b.load()
		require.NoError(err)
		_, err = signer.advanceCutover(s)
		require.NoError(err)
		return signer.cutoverLeft(s)[chainDogecoin]
	}
	require.Contains(left(), op, "conflict 3 deep: still owed")
	core.listed[wid] = depth - 1
	require.Contains(left(), op, "one short of rootDepth: still owed")
	core.listed[wid] = depth
	require.NotContains(left(), op, "rootDepth deep: cleared")

	signer.b.doge = coreConflicts{fakeChain: h.doge, core: node()} // a restart
	require.NotContains(left(), op, "still cleared after a restart")

	core.listed[wid] = depth - 1 // a reorganisation: the evidence is shallow again
	require.Contains(left(), op, "owed again")
	core.listed[wid] = depth
	require.NotContains(left(), op)
}

// TestCoinbasesAreNotConflictEvidence (X3, Codex r8 #4): every coinbase
// spends the same null outpoint, so two unrelated coinbases "share" an
// input; that is never evidence one can't be mined.
func TestCoinbasesAreNotConflictEvidence(t *testing.T) {
	require := require.New(t)
	coinbase := func(tag byte) *wire.MsgTx {
		tx := wire.NewMsgTx(1)
		tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{}, wire.MaxPrevOutIndex), []byte{tag}, nil))
		tx.AddTxOut(wire.NewTxOut(1, []byte{0x51}))
		return tx
	}
	require.False(spendsInputOf(coinbase(1), coinbase(2)))
}
