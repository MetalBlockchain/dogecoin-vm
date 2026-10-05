package main

import (
	"net/http"
	"path/filepath"
	"strings"
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

// TestDogeFenceFreezesOtherSurplusSpendsAcrossQuorums (X2, both r8
// reviews): A and B sign a Dogecoin fence while C is down, so only their
// logs hold it (and any costlier replacement of it). While it is shallower
// than rootDepth, a reorganisation could mine a costlier alternative in its
// place. The coordinator then turns to B and C for a DogecoinVM fence: C
// never signed anything on Dogecoin, so its own reservations are empty, and
// in round 7 it signed, letting fees add up past the surplus. Now every
// signer that sees a Dogecoin fence short of rootDepth refuses every other
// surplus spend, whoever signed the fence; once it is deep, B and C fence
// DogecoinVM.
func TestDogeFenceFreezesOtherSurplusSpendsAcrossQuorums(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	_, _, _, _ = h.inUse()
	h.rebuildLogs()
	down := func(i int) {
		h.servers[i].Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "down", http.StatusServiceUnavailable)
		})
	}
	up := func(i int) { h.servers[i].Config.Handler = h.signers[i].handler() }
	a, c := 0, 2

	down(c)
	_, err := h.b.proposeFence(chainDogecoin)
	require.NoError(err, "A and B sign the Dogecoin fence")
	h.doge.mine() // in a block, one deep
	for key := range h.signers[c].log.Actions {
		require.False(strings.HasPrefix(key, actionFence+":") && strings.Contains(key, chainDogecoin+":"), "C never signed it: %s", key)
	}

	up(c)
	down(a)
	_, err = h.b.proposeFence(chainDogecoinVM)
	require.ErrorContains(err, h.servers[c].URL+": a Dogecoin fence", "C refuses, though its own log reserves nothing")
	require.ErrorContains(err, "no other fee is paid from the surplus")

	s, err := h.signers[c].b.load()
	require.NoError(err)
	for range s.pegLine.rootDepth {
		h.doge.mine()
	}
	_, err = h.b.proposeFence(chainDogecoinVM)
	require.NoError(err, "the Dogecoin fence is deep: B and C fence DogecoinVM")
}

// TestGoneInputIsDeadOnlyWhenSpentDeep (X2, Codex r8 #1): a signed fence
// whose input is missing from this signer's view and its node's UTXO set
// was taken as dead, freeing its fee, even when what spent the input was a
// Dogecoin transaction a reorganisation could still undo. Now only a spend
// rootDepth deep frees it.
func TestGoneInputIsDeadOnlyWhenSpentDeep(t *testing.T) {
	require := require.New(t)
	in := wire.OutPoint{Hash: chainhash.Hash{0x22}, Index: 0}
	log := &signingLog{Actions: map[string]*loggedAction{
		"fence:dogecoin:x": {Txs: []loggedTx{{Txid: "3333333333333333333333333333333333333333333333333333333333333333", Chain: chainDogecoin, Fee: 200_000_000, Inputs: []string{in.String()}}}},
	}}
	spender := wire.NewMsgTx(1)
	spender.AddTxIn(wire.NewTxIn(&in, nil, nil))
	ev := func(confs int64) *chainEvidence {
		return &chainEvidence{deep: 20, listed: map[string]map[string]chainTx{
			chainDogecoin: {unsignedHash(spender).String(): {tx: spender, confirmations: confs}},
		}}
	}
	gone := func(string, wire.OutPoint) bool { return false }
	require.EqualValues(200_000_000, log.unseenFees(nil, ev(3), gone, ""), "spent 3 deep: still reserved")
	require.EqualValues(200_000_000, log.unseenFees(nil, &chainEvidence{deep: 20}, gone, ""), "spent by nothing it can see: still reserved")
	require.Zero(log.unseenFees(nil, ev(20), gone, ""), "spent rootDepth deep: dead")
}

// TestInventoryGatesAdoption (X5, both r8 reviews): before adopting, the
// operator sees every coin with its depth, the fences they take and what
// they cost, and the surplus they need; it is ready only when every coin is
// rootDepth deep and the surplus covers twice the fences' fees and headroom.
func TestInventoryGatesAdoption(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	inv := func() inventory {
		s, err := h.b.load()
		require.NoError(err)
		inv, err := nodeInventory(h.b, s)
		require.NoError(err)
		return inv
	}
	got := inv()
	require.False(got.Ready)
	require.Equal(1, got.Chains[chainDogecoin].Batches)
	require.Contains(strings.Join(got.Problems, "\n"), "contribute (DVMC) first", "1 DOGE of surplus doesn't cover the fences")

	h.contribute(20*doge, 100)
	got = inv()
	require.True(got.Ready, "%v", got.Problems)
	require.Len(got.Chains[chainDogecoin].Coins, 2)

	shallow := h.contribute(doge, 2)
	got = inv()
	require.False(got.Ready)
	require.Contains(strings.Join(got.Problems, "\n"), shallow.TxHash().String()+":0 has 2 of")
}
