package main

import (
	"net/http"
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

// TestCutoverBudgetCountsEveryFenceOnce (Y1, Codex r9 #1): over a cutover a
// signer pays at most its budget in fence fees, spent or not, each action
// at its costliest alternative; a fence from before the cutover doesn't
// count.
func TestCutoverBudgetCountsEveryFenceOnce(t *testing.T) {
	require := require.New(t)
	l := &signingLog{CutoverEpoch: 2, CutoverStarted: 1000, Actions: map[string]*loggedAction{
		"fence:a:0":  {First: 1000, Txs: []loggedTx{{Fee: 30}, {Fee: 50}}},
		"fence:b:0":  {First: 1200, Txs: []loggedTx{{Fee: 40}}},
		"fence:c:0":  {First: 900, Txs: []loggedTx{{Fee: 1000}}}, // earlier cutover
		"cancel:d:0": {First: 1100, Txs: []loggedTx{{Fee: 500}}}, // not a fence
	}}
	require.EqualValues(100, l.epochFenceFees("fence:a:0", 60), "a replacement counts at its costliest")
	require.EqualValues(90, l.epochFenceFees("fence:a:0", 10), "a cheaper one adds nothing")
	require.EqualValues(100, l.epochFenceFees("fence:e:0", 10), "a new action adds its fee")
}

// TestCutoverBudgetStopsCumulativeFees (Y1, Codex r9 #1): with one
// dishonest signer the coordinator can route every DogecoinVM fence to the
// honest signer that knows nothing of another quorum's kept-back fence.
// feeRoom alone lets it sign fee after fee as each becomes final; the
// budget stops it at its share of the surplus it saw when the cutover
// began.
func TestCutoverBudgetStopsCumulativeFees(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.inUse()
	h.rebuildLogs()
	c := h.signers[2]
	s, err := c.b.load()
	require.NoError(err)
	key := actionFence + ":" + wire.OutPoint{Index: 7}.String()
	require.NoError(c.cutoverBudgetRoom(s, key, 1))
	budget := c.log.CutoverBudget
	require.Positive(budget)
	require.Equal(c.log.CutoverEpoch, c.log.CutoverBudgetEpoch)

	c.log.Actions["fence:spent:0"] = &loggedAction{First: c.log.CutoverStarted, Txs: []loggedTx{{Fee: budget - 5}}}
	require.NoError(c.cutoverBudgetRoom(s, key, 5), "up to the budget")
	require.ErrorContains(c.cutoverBudgetRoom(s, key, 6), "more than its budget")
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
