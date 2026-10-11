package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
	"github.com/stretchr/testify/require"
)

// minedButMissing sets up a signer whose wallet listed a deposit P in a
// block (3 deep, so the unsettled file keeps it), returning P, its stub
// Dogecoin Core, and a function that reloads the signer's state.
func minedButMissing(t *testing.T) (*cosignHarness, *cosigner, *wire.MsgTx, *stubDogecoinCore, destination) {
	h := newCosignHarness(t)
	signer := h.signers[0]
	s, err := signer.b.load()
	require.NoError(t, err)
	alice := h.user(1)
	p := h.deposit(5*doge, &alice, 30) // in the chain, deep
	pid := p.TxHash().String()
	core := &stubDogecoinCore{listed: map[string]int64{pid: 3}, bodies: map[string]string{pid: encodeTx(p)}}
	srv := core.serve(t)
	path := filepath.Join(t.TempDir(), deadHistoryFile)
	signer.b.doge = coreConflicts{fakeChain: h.doge, core: &dogeChain{rpc: newRPCClient(srv.URL, "", ""), dead: &deadHistory{path: path, depth: s.pegLine.rootDepth}}}
	_, err = signer.b.load() // the wallet lists P in a block: recorded, mined
	require.NoError(t, err)
	return h, signer, p, core, alice
}

// TestMinedButMissingWithAListedSpenderRefuses (AA2, Codex r11 #2): the
// wallet lost deposit P, but still lists F, a block transaction spending
// P's output. Round 10 put P back as conflicted (so out of the books) and
// let F's spend clear P's cutover coin: the customer's claim vanished and
// the surplus was overstated. Now nothing is read, so nothing is signed or
// accounted, until P is imported; after the import P's claim is back.
func TestMinedButMissingWithAListedSpenderRefuses(t *testing.T) {
	require := require.New(t)
	h, signer, p, core, alice := minedButMissing(t)
	pid := p.TxHash().String()

	// A release of P, built while the wallet still lists it.
	s0, err := signer.b.load()
	require.NoError(err)
	d0, ok := findDeposit(s0.deposits, wire.OutPoint{Hash: p.TxHash(), Index: 0})
	require.True(ok)
	inputs, total, err := s0.releaseInputs(d0.value)
	require.NoError(err)
	release := signRequest{Chain: chainDogecoinVM, Tx: encodeTx(h.b.buildRelease(inputs, total, d0)),
		Register: []string{encodeDest(alice)}, Action: action{Kind: actionRelease, Deposit: d0.outPoint.String()}}

	f := wire.NewMsgTx(1)
	f.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: p.TxHash(), Index: 0}, nil, nil))
	f.AddTxOut(wire.NewTxOut(5*doge-doge/10, h.b.signers.pkScript()))
	fid := f.TxHash().String()
	delete(core.listed, pid) // the wallet lost P
	core.listed[fid] = 3
	core.bodies[fid] = encodeTx(f)

	_, err = signer.b.load()
	require.ErrorContains(err, "was mined")
	require.ErrorContains(err, fid, "names the spender it saw")
	_, _, _, err = signer.check(release)
	require.ErrorContains(err, "was mined", "and a signature is refused")

	core.listed[pid] = 3 // imported
	s, err := signer.b.load()
	require.NoError(err)
	d, ok := findDeposit(s.deposits, wire.OutPoint{Hash: p.TxHash(), Index: 0})
	require.True(ok, "P's deposit is back in the books")
	require.Equal(int64(5*doge), d.value)
	a := signer.b.audit(s)
	require.GreaterOrEqual(a.PendingPegIns, int64(5*doge), "and owed")
	require.Equal(a.Locked-a.Required, a.Surplus)
}

// TestMinedButMissingWithNoSpenderRefuses (AA2): the wallet lost P and
// lists nothing spending it, but it once listed P in a block: P was mined,
// not dropped, so the same refusal until it is imported.
func TestMinedButMissingWithNoSpenderRefuses(t *testing.T) {
	require := require.New(t)
	_, signer, p, core, _ := minedButMissing(t)
	pid := p.TxHash().String()
	delete(core.listed, pid)
	_, err := signer.b.load()
	require.ErrorContains(err, "the wallet listed it in a block")

	core.listed[pid] = 3
	_, err = signer.b.load()
	require.NoError(err, "imported: read again")
}

// TestDroppedUnminedTransactionStillOnlyGates (AA2's boundary): a
// transaction the wallet only ever listed unconfirmed and then dropped was
// never mined, and nothing spends it: it isn't a missing claim, so reading
// goes on (its coins stay owed in a cutover, as before).
func TestDroppedUnminedTransactionStillOnlyGates(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	signer := h.signers[0]
	s, err := signer.b.load()
	require.NoError(err)
	tx := h.contribute(doge, 0)
	id := tx.TxHash().String()
	core := &stubDogecoinCore{listed: map[string]int64{id: 0}, bodies: map[string]string{id: encodeTx(tx)}}
	srv := core.serve(t)
	path := filepath.Join(t.TempDir(), deadHistoryFile)
	signer.b.doge = coreConflicts{fakeChain: h.doge, core: &dogeChain{rpc: newRPCClient(srv.URL, "", ""), dead: &deadHistory{path: path, depth: s.pegLine.rootDepth}}}
	_, err = signer.b.load()
	require.NoError(err)
	delete(core.listed, id)
	_, err = signer.b.load()
	require.NoError(err)
}

// TestLaterCutoverOfAFencedLogRefuses (AA1, Codex r11 #1): once a log is
// fenced, this release won't start another cutover on it. A later cutover
// runs while the other signers sign as usual, and their cancellations spend
// the surplus a fence kept back from it could still claim.
func TestLaterCutoverOfAFencedLogRefuses(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.inUse()
	h.rebuildLogs()
	h.fenceUntilDone(40)
	for _, c := range h.signers {
		require.Equal(historyFenced, c.log.History)
		epoch := c.log.CutoverEpoch
		s, err := c.b.load()
		require.NoError(err)
		require.ErrorContains(c.startCutover(s), "later cutover isn't supported")
		require.Equal(epoch, c.log.CutoverEpoch, "nothing started")
		require.Empty(c.log.Cutover)
	}
}

// TestCancellationsCountAgainstTheLifetimeAllowance (AA1): a cancellation's
// fee comes out of the same surplus as a fence's, so it counts against the
// same lifetime allowance; every honest signer staying within its share is
// what bounds all quorums' spends together. Round 10 checked cancellations
// only against the visible surplus, so cancellations confirmed one at a
// time could eat the room a kept-back fence still needed.
func TestCancellationsCountAgainstTheLifetimeAllowance(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.setPolicy(func(b *bridge) { b.vmFee = doge / 10 })
	alice := h.user(1)
	dep := h.deposit(3*doge, &alice, 6)
	op := wire.OutPoint{Hash: dep.TxHash()}
	h.setPolicy(func(b *bridge) { b.minDeposit = 4 * doge }) // held, with a destination
	addr, err := h.user(9).address(h.b.dogeParams)
	require.NoError(err)
	for _, c := range h.signers {
		path := filepath.Join(t.TempDir(), "approvals")
		require.NoError(os.WriteFile(path, []byte(op.String()+" "+addr.EncodeAddress()+"\n"), 0o600))
		c.refundApprovals = path
	}
	s, err := h.b.load()
	require.NoError(err)
	inputs, err := s.cancelInputs(h.b.vmFee)
	require.NoError(err)
	tx, err := h.b.buildCancel(inputs, op)
	require.NoError(err)
	req := signRequest{Chain: chainDogecoinVM, Action: action{Kind: actionCancel, Deposit: op.String()}, Tx: encodeTx(tx)}

	// C's allowance is nearly spent: half a fee left.
	c := h.signers[2]
	c.log.CutoverEpoch = 1
	c.log.FenceBudget = h.b.vmFee / 2
	c.log.FenceCapital = nil
	for _, u := range s.contributions {
		c.log.FenceCapital = append(c.log.FenceCapital, u.outPoint.String())
	}
	_, _, _, err = c.check(req)
	require.ErrorContains(err, "more than its budget", "a cancellation counts against the allowance")

	c.log.FenceBudget = 10 * doge
	_, _, _, err = c.check(req)
	require.NoError(err, "within the allowance it signs")
}
