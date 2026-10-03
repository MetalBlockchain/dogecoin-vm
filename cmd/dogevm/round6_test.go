package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// Round 6 (V1, V5, V6, V9): the parts of the shared cutover DogecoinVM still
// did its own way.

// adoptOne rebuilds one signer's log from the chains, which starts its
// cutover now.
func (h *cosignHarness) adoptOne(c *cosigner) {
	h.t.Helper()
	c.log = &signingLog{path: filepath.Join(h.t.TempDir(), "signing-log.json"), Actions: map[string]*loggedAction{}}
	s, err := c.b.load()
	require.NoError(h.t, err)
	_, err = c.adopt(s)
	require.NoError(h.t, err)
	require.Contains(h.t, c.log.Cutover, chainDogecoin)
}

// fenceUntil proposes fences on both chains and mines until every signer in
// done has finished its cutover.
func (h *cosignHarness) fenceUntil(rounds int, done ...*cosigner) {
	h.t.Helper()
	for range rounds {
		for _, chain := range cutoverChains {
			_, _ = h.b.proposeFence(chain)
		}
		h.vm.mine()
		h.doge.mine()
		all := true
		for _, c := range done {
			s, err := c.b.load()
			require.NoError(h.t, err)
			_, err = c.advanceCutover(s)
			require.NoError(h.t, err)
			all = all && !c.log.unproven()
		}
		if all {
			return
		}
	}
	h.t.Fatalf("the cutover didn't finish in %d rounds", rounds)
}

// TestHonestQuorumFencesFromDifferentSnapshots (Codex r6 #5, V5): two
// honest signers whose cutovers began at different times record different
// coins (a deposit arrived in between). With the third signer refusing,
// they still agree on one fence, because fence inputs come from the chain;
// round 5 built each fence from the recorded list and wanted Required
// signers to report the same one, so A+C could never fence.
func TestHonestQuorumFencesFromDifferentSnapshots(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	_, _, _, _ = h.inUse()
	a, b, c := h.signers[0], h.signers[1], h.signers[2]
	h.adoptOne(a)
	late := h.contribute(3*doge, 30) // after A's cutover began, before C's
	h.adoptOne(c)
	s, err := a.b.load()
	require.NoError(err)
	require.NotEqual(a.cutoverLeft(s)[chainDogecoin], c.cutoverLeft(s)[chainDogecoin], "the snapshots differ")
	require.Contains(c.log.Cutover[chainDogecoin], late.TxHash().String()+":0")
	_ = b
	h.servers[1].Close() // B refuses everything

	h.fenceUntil(30, a, c)
}

// TestVMFenceFeeIsSizeBased (V5): a DogecoinVM fence pays the shared rule,
// max(vmFee, ceil(size * vmFee / 250)), for its real signed size, so a big
// reserve's fence clears the relay floor (0.001 DOGE per kB) however many
// coins it spends.
func TestVMFenceFeeIsSizeBased(t *testing.T) {
	require := require.New(t)
	const floorPerKB = doge / 1000
	redeem := make([]byte, 105) // a 2-of-3 multisig redeem script
	for _, inputs := range []int{1, 10, 100, 400} {
		redeems := make([][]byte, inputs)
		for i := range redeems {
			redeems[i] = redeem
		}
		size := fenceSize(redeems, 2)
		require.Greater(size, int64(inputs*290), "each input carries two signatures and the script")
		for _, base := range []int64{doge / 100, doge / 1000} {
			fee := fenceFee(chainDogecoinVM, base, size)
			require.GreaterOrEqual(fee, base)
			require.GreaterOrEqual(fee*1000, size*floorPerKB, "%d inputs, vmFee %d: under the relay floor", inputs, base)
		}
	}
}

// TestCoinOnlyOneSignerWatchesKeepsItGated (V9; Grok r6 "untested"): a
// deposit address registered on one signer only. That signer records the
// coin; the coordinator's fences don't spend it, so the other two finish
// and that signer stays in its cutover (fail closed), signing nothing but
// fences. Recovery: register the address with the coordinator and start a
// new epoch on the others, so they sign the fence that spends it.
func TestCoinOnlyOneSignerWatchesKeepsItGated(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	_, _, _, _ = h.inUse()
	a, b, c := h.signers[0], h.signers[1], h.signers[2]
	carol := h.user(7)
	_, err := registerDeposit(a.b, carol)
	require.NoError(err)
	dep := h.personalDeposit(2*doge, carol, 30)
	coin := wire.OutPoint{Hash: dep.TxHash()}.String()
	h.rebuildLogs()
	require.Contains(a.log.Cutover[chainDogecoin], coin, "the signer that watches it records it")
	require.NotContains(b.log.Cutover[chainDogecoin], coin)

	h.fenceUntil(30, b, c)
	s, err := a.b.load()
	require.NoError(err)
	_, err = a.advanceCutover(s)
	require.NoError(err)
	require.Equal([]string{coin}, a.cutoverLeft(s)[chainDogecoin], "A still waits for its coin")
	require.True(a.log.unproven(), "and signs nothing but fences")

	_, err = registerDeposit(h.b, carol)
	require.NoError(err)
	for _, other := range []*cosigner{b, c} {
		s, err := other.b.load()
		require.NoError(err)
		require.NoError(other.startCutover(s))
	}
	h.fenceUntil(30, a, b, c)
}

// TestMixedHistoryQuorum (V9; Grok r6 "untested"): one signer new (init: its
// key never signed), one fenced, one adopted and still in its cutover. The
// new and the fenced one sign a release; the one in a cutover refuses it,
// on either chain, until its own cutover is done.
func TestMixedHistoryQuorum(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	_, _, _, _ = h.inUse()
	a, b, c := h.signers[0], h.signers[1], h.signers[2]
	alice := h.user(1)
	h.adoptOne(b)
	h.adoptOne(c)
	h.fenceUntil(30, b, c)
	require.Equal(historyFenced, c.log.History)
	h.adoptOne(b) // B again unproven, in a new cutover
	require.Equal(historyNew, a.log.History)

	dep := h.deposit(4*doge, &alice, 30)
	require.NoError(h.releaseErr(a, dep, alice), "new: signs")
	require.NoError(h.releaseErr(c, dep, alice), "fenced: signs")
	require.ErrorContains(h.releaseErr(b, dep, alice), "in a cutover", "in a cutover: refuses")
}
