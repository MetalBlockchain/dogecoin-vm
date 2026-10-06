package main

import (
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// Round 9 (Y1): the cross-quorum fee freeze, for fences an honest signer
// can't see.

// quorum lets the test take a signer down and bring it back.
func (h *cosignHarness) down(i int) {
	h.servers[i].Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
}

func (h *cosignHarness) up(i int) { h.servers[i].Config.Handler = h.signers[i].handler() }

// takeDogeFence removes the set's Dogecoin fence from the chain and returns
// it: a coordinator keeping it back, or a node that dropped it.
func (h *cosignHarness) takeDogeFence() *wire.MsgTx {
	h.t.Helper()
	kept := h.doge.txs[:0]
	var fence *wire.MsgTx
	for _, t := range h.doge.txs {
		if _, ok := parseFence(t.tx); ok && fence == nil {
			fence = t.tx
			continue
		}
		kept = append(kept, t)
	}
	h.doge.txs = kept
	require.NotNil(h.t, fence, "a Dogecoin fence was sent")
	return fence
}

// TestKeptBackDogeFenceFreezesTheOtherQuorum (Y1, both r9 reviews): A and B
// sign a Dogecoin fence the coordinator keeps back (or a 5-deep one a
// reorganisation drops). C never saw it and its log reserves nothing; round
// 8's freeze saw no fence, so B and C could pay a DogecoinVM fee while the
// kept-back fence could still be mined. Now C refuses while its own
// Dogecoin cutover has coins left, and fences DogecoinVM once they clear.
func TestKeptBackDogeFenceFreezesTheOtherQuorum(t *testing.T) {
	for _, tc := range []struct {
		name  string
		depth int
	}{{"kept back", 0}, {"5 deep, then reorganised out", 5}} {
		t.Run(tc.name, func(t *testing.T) {
			require := require.New(t)
			h := newCosignHarness(t)
			h.inUse()
			h.rebuildLogs()
			a, c := 0, 2

			h.down(c)
			_, err := h.b.proposeFence(chainDogecoin)
			require.NoError(err, "A and B sign the Dogecoin fence")
			for range tc.depth {
				h.doge.mine()
			}
			fence := h.takeDogeFence()

			h.up(c)
			h.down(a)
			_, err = h.b.proposeFence(chainDogecoinVM)
			require.ErrorContains(err, h.servers[c].URL+": this signer's Dogecoin cutover still has", "C refuses")

			// The fence comes back and is mined deep: C's coins clear.
			h.up(a)
			_, err = h.doge.send(fence)
			require.NoError(err)
			s, err := h.signers[c].b.load()
			require.NoError(err)
			for range s.pegLine.rootDepth {
				h.doge.mine()
			}
			h.down(a)
			_, err = h.b.proposeFence(chainDogecoinVM)
			require.NoError(err, "C's Dogecoin cutover is done: B and C fence DogecoinVM")
		})
	}
}

// TestDroppedDogeFenceIsStillUnsettled (Y1, Grok r9 #1): Core 1.14 lists a
// transaction dropped from its mempool as conflicted at -1. It can still be
// sent and mined, so the freeze holds; round 8 looked only at the
// non-conflicted list. It lifts once what conflicts with it is deep.
func TestDroppedDogeFenceIsStillUnsettled(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.inUse()
	h.rebuildLogs()
	_, err := h.b.proposeFence(chainDogecoin)
	require.NoError(err)
	fence := h.takeDogeFence()

	s, err := h.signers[2].b.load()
	require.NoError(err)
	require.Nil(unsettledDogeFence(s), "gone from the wallet's list: nothing seen")
	s.dogeConflicted = append(s.dogeConflicted, chainTx{tx: fence, confirmations: -1})
	got := unsettledDogeFence(s)
	require.NotNil(got, "listed at -1: still unsettled")
	require.ErrorContains(waitForDogeFence(s), "out of the mempool but not dead")

	s.dogeConflicted[len(s.dogeConflicted)-1].confirmations = -s.pegLine.rootDepth
	require.Nil(unsettledDogeFence(s), "what conflicts with it is rootDepth deep: dead")
}

// TestFenceBudgetCountsEveryFenceOnce (Y1, Z1): a signer's fence fees are
// counted once per action at its costliest alternative, over its whole
// life: a fence from an earlier cutover still counts.
func TestFenceBudgetCountsEveryFenceOnce(t *testing.T) {
	require := require.New(t)
	l := &signingLog{CutoverEpoch: 2, CutoverStarted: 1000, Actions: map[string]*loggedAction{
		"fence:a:0":  {First: 1000, Txs: []loggedTx{{Fee: 30}, {Fee: 50}}},
		"fence:b:0":  {First: 1200, Txs: []loggedTx{{Fee: 40}}},
		"fence:c:0":  {First: 900, Txs: []loggedTx{{Fee: 1000}}}, // an earlier cutover: still counts
		"cancel:d:0": {First: 1100, Txs: []loggedTx{{Fee: 500}}}, // not a fence
	}}
	require.EqualValues(1100, l.fenceFees("fence:a:0", 60), "a replacement counts at its costliest")
	require.EqualValues(1090, l.fenceFees("fence:a:0", 10), "a cheaper one adds nothing")
	require.EqualValues(1100, l.fenceFees("fence:e:0", 10), "a new action adds its fee")
}

// TestFenceBudgetStopsCumulativeFees (Y1): with one dishonest signer the
// coordinator can route every fence to the honest signer that knows nothing
// of another quorum's kept-back fence; the budget stops it at its share.
func TestFenceBudgetStopsCumulativeFees(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.inUse()
	h.rebuildLogs()
	c := h.signers[2]
	s, err := c.b.load()
	require.NoError(err)
	key := actionFence + ":" + wire.OutPoint{Index: 7}.String()
	require.NoError(c.cutoverBudgetRoom(s, key, 1))
	budget := c.log.FenceBudget
	require.Positive(budget)

	c.log.Actions["fence:spent:0"] = &loggedAction{First: c.log.CutoverStarted, Txs: []loggedTx{{Fee: budget - 5}}}
	require.NoError(c.cutoverBudgetRoom(s, key, 5), "up to the budget")
	require.ErrorContains(c.cutoverBudgetRoom(s, key, 6), "more than its budget")
}

// TestNewCutoverDoesNotRefillTheFenceBudget (Z1, Codex r10 #1): A and B
// sign a DogecoinVM fence and the coordinator keeps it back from C. C
// starts a new cutover. Round 9 gave each cutover a fresh budget, so C
// could pay new Dogecoin fees while the kept-back fence (charged to the
// earlier cutover) could still be mined: together more than the surplus.
// The budget is now lifetime: a new cutover adds nothing, and only a new
// contribution, once rootDepth deep, adds C's share of it.
func TestNewCutoverDoesNotRefillTheFenceBudget(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.inUse()
	h.rebuildLogs()
	c := h.signers[2]
	s, err := c.b.load()
	require.NoError(err)
	key := actionFence + ":" + wire.OutPoint{Index: 9}.String()
	require.NoError(c.cutoverBudgetRoom(s, key, 1))
	budget := c.log.FenceBudget
	require.Positive(budget)
	// C signed fences up to its budget in this cutover; one of them was a
	// DogecoinVM fence kept back.
	c.log.Actions["fence:keptback:0"] = &loggedAction{First: c.log.CutoverStarted, Txs: []loggedTx{{Chain: chainDogecoinVM, Fee: budget}}}

	// A new cutover.
	before := c.log.CutoverEpoch
	require.NoError(c.startCutover(s))
	require.Equal(before+1, c.log.CutoverEpoch)
	s, err = c.b.load()
	require.NoError(err)
	require.ErrorContains(c.cutoverBudgetRoom(s, key, 1), "more than its budget", "a new cutover refills nothing")
	require.Equal(budget, c.log.FenceBudget)

	// New capital adds room only once it is deep.
	h.contribute(4*doge, 1)
	s, err = c.b.load()
	require.NoError(err)
	require.ErrorContains(c.cutoverBudgetRoom(s, key, 1), "more than its budget", "a shallow contribution adds nothing")
	for range s.pegLine.rootDepth {
		h.doge.mine()
	}
	s, err = c.b.load()
	require.NoError(err)
	require.NoError(c.cutoverBudgetRoom(s, key, 1), "a deep one adds this signer's share")
	require.Greater(c.log.FenceBudget, budget)
}

// TestPendingVMPaymentNeverHoldsTheCutover (Y2, Codex r9 #2): a payment to
// the reserve that isn't in a DogecoinVM block yet makes the inventory not
// ready, and isn't recorded by a cutover: no signature from before could
// spend it (signers spent only confirmed reserve coins, and DogecoinVM
// blocks are final). Round 8 recorded it; once it was replaced by a
// payment elsewhere, the cutover could never clear it.
func TestPendingVMPaymentNeverHoldsTheCutover(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.inUse()
	h.contribute(20*doge, 100)
	pending := wire.NewMsgTx(1)
	pending.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
	pending.AddTxOut(wire.NewTxOut(doge, h.b.signers.pkScript()))
	h.vm.add(pending, 0)

	s, err := h.b.load()
	require.NoError(err)
	inv, err := nodeInventory(h.b, s)
	require.NoError(err)
	require.False(inv.Ready)
	require.Contains(strings.Join(inv.Problems, "\n"), pending.TxHash().String()+" pays the set and is not in a block yet")

	h.rebuildLogs()
	c := h.signers[2]
	require.NotContains(c.log.Cutover[chainDogecoinVM], wire.OutPoint{Hash: pending.TxHash()}.String(), "not recorded")

	// It is replaced by a payment elsewhere, which is final.
	kept := h.vm.txs[:0]
	for _, t := range h.vm.txs {
		if t.tx.TxHash() != pending.TxHash() {
			kept = append(kept, t)
		}
	}
	h.vm.txs = kept
	h.fenceUntilDone(30)
	require.Equal(historyFenced, c.log.History, "the cutover finishes")
}

// TestRememberedConflictClearsNothing (Y6, Grok r9 #2): a dropped
// transaction the wallet once listed conflicted rootDepth deep was kept
// cleared from memory (its "settled" flag) after the wallet stopped
// listing it and anything that conflicts with it. Now only evidence read
// now clears its coin.
func TestRememberedConflictClearsNothing(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	_, _, _, _ = h.inUse()
	signer := h.signers[0]
	s, err := signer.b.load()
	require.NoError(err)
	depth := s.pegLine.rootDepth

	dropped := wire.NewMsgTx(1)
	dropped.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
	dropped.AddTxOut(wire.NewTxOut(2*doge, h.b.signers.pkScript()))
	dropped.AddTxOut(nullData(tagContrib))
	id := dropped.TxHash().String()
	op := wire.OutPoint{Hash: dropped.TxHash()}.String()
	core := &stubDogecoinCore{listed: map[string]int64{id: 0}, bodies: map[string]string{id: encodeTx(dropped)}}
	srv := core.serve(t)
	path := filepath.Join(t.TempDir(), deadHistoryFile)
	node := &dogeChain{rpc: newRPCClient(srv.URL, "", ""), dead: &deadHistory{path: path, depth: depth}}
	signer.b.doge = coreConflicts{fakeChain: h.doge, core: node}
	_, err = signer.b.load() // listed, unconfirmed: kept in the file
	require.NoError(err)
	core.listed[id] = -3 // conflicted 3 deep
	h.adoptOne(signer)
	require.Contains(signer.log.Cutover[chainDogecoin], op)

	left := func() []string {
		s, err := signer.b.load()
		require.NoError(err)
		_, err = signer.advanceCutover(s)
		require.NoError(err)
		return signer.cutoverLeft(s)[chainDogecoin]
	}
	core.listed[id] = -depth // the wallet lists it conflicted rootDepth deep
	require.NotContains(left(), op, "evidence read now: cleared")
	delete(core.listed, id) // the wallet drops it, and lists nothing that conflicts
	require.Contains(left(), op, "memory alone clears nothing")
}

// TestInventoryCountsTheCarriedChange (Y4, both r9 reviews): every fence
// after the first starts with the change the one before paid back, so 200
// coins at 100 inputs a fence take three fences, not two; and a stuck
// Dogecoin fence is replaced one fence fee higher each time (its own
// size-based fee, not one base fee). The inventory now says so, and the
// cutover really takes that many.
func TestInventoryCountsTheCarriedChange(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	per := maxFenceInputs(chainDogecoin)
	for range 2*per - 1 { // plus the seed: 2*per coins
		h.contribute(doge, 100)
	}
	s, err := h.b.load()
	require.NoError(err)
	inv, err := nodeInventory(h.b, s)
	require.NoError(err)
	ic := inv.Chains[chainDogecoin]
	require.Len(ic.Coins, 2*per)
	require.Equal(3, ic.Batches, "the change takes a place in every later batch")

	_, _, costliest, infeasible, err := h.b.fenceBudget(s, chainDogecoin)
	require.NoError(err)
	require.Empty(infeasible)
	require.Greater(costliest, h.b.fenceBase(chainDogecoin), "a 100-input fence costs more than one base fee")
	require.Equal(formatDoge(int64(fenceReplacements)*costliest), ic.Headroom)

	h.rebuildLogs()
	h.fenceUntilDone(80)
	fences := 0
	for _, t := range h.doge.txs {
		if _, ok := parseFence(t.tx); ok {
			fences++
		}
	}
	require.Equal(ic.Batches, fences, "the cutover took as many Dogecoin fences as the inventory said")
}
