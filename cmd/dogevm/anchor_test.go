package main

import (
	"os"
	"testing"
	"time"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
	"github.com/stretchr/testify/require"
)

// payoutRequest is a sign request for paying peg-out req from inputs.
func (h *cosignHarness) payoutRequest(req *wire.MsgTx, inputs []utxo) signRequest {
	h.t.Helper()
	s, err := h.b.load()
	require.NoError(h.t, err)
	p, ok := findPegOut(s.pegOuts, req.TxHash())
	require.True(h.t, ok)
	tx, err := h.b.buildPayout(inputs, p.value, p.dest, encodePayment(p.txid))
	require.NoError(h.t, err)
	return signRequest{Chain: chainDogecoin, Action: action{Kind: actionPayout, PegOut: p.txid.String()}, Tx: encodeTx(tx)}
}

// TestColludingSignerCantDoublePay: the coordinator and signer B get A and
// B to sign a payout and keep it back, then ask C for the same payout from
// other coins. Neither honest signer would sign twice, so without the
// anchor both payouts could confirm. C must refuse anything that doesn't
// spend the peg's anchor, which the kept-back payout already spends, even
// after a new deposit or a decoy spend of the anchor shown to its node.
func TestColludingSignerCantDoublePay(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice := h.user(1)
	h.deposit(10*doge, &alice, 9)
	h.deposit(10*doge, &alice, 8)
	for h.step() != "" {
		h.vm.mine()
	}
	req := h.pegOut(5*doge, h.user(2))
	s, err := h.b.load()
	require.NoError(err)
	require.NotNil(s.pegAnchor)
	require.Equal(int64(seedValue), s.pegAnchor.value, "the seed at the peg address")
	var first, other utxo
	for _, u := range s.lockedUTXOs {
		if u.confirmations > 0 && u.outPoint != s.pegAnchor.outPoint && u.value == 10*doge {
			if first.value == 0 {
				first = u
			} else {
				other = u
			}
		}
	}
	require.NotZero(other.value)
	sign := func(i int, r signRequest) error {
		return h.b.cosigners[i].post("/v1/sign", r, &signResponse{})
	}
	kept := h.payoutRequest(req, []utxo{*s.pegAnchor, first})
	require.NoError(sign(0, kept))
	require.NoError(sign(1, kept))

	require.ErrorContains(sign(2, h.payoutRequest(req, []utxo{other})), "the oldest confirmed coin")
	h.deposit(20*doge, &alice, 1)
	require.ErrorContains(sign(2, h.payoutRequest(req, []utxo{other})), "the oldest confirmed coin")
	decoy := wire.NewMsgTx(1)
	decoy.AddTxIn(wire.NewTxIn(&s.pegAnchor.outPoint, nil, nil))
	decoy.AddTxOut(wire.NewTxOut(s.pegAnchor.value-pegDust, h.b.signers.pkScript()))
	h.doge.add(decoy, 0)
	require.ErrorContains(sign(2, h.payoutRequest(req, []utxo{other})), "the oldest confirmed coin")
}

// TestLateRegisteredDepositCantMoveTheAnchor: a personal deposit address
// paid in an older block and registered (at C) only later must not become
// C's anchor, or C and a colluding B could pay a withdrawal again beside
// one A and B signed.
func TestLateRegisteredDepositCantMoveTheAnchor(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice, mallory := h.user(1), h.user(7)
	h.deposit(10*doge, &alice, 9)
	for h.step() != "" {
		h.vm.mine()
	}
	old := h.personalDeposit(20*doge, mallory, 500) // older than the seed
	oldCoin := utxo{outPoint: wire.OutPoint{Hash: old.TxHash()}, value: 20 * doge,
		pkScript: old.TxOut[0].PkScript, confirmations: 500}
	req := h.pegOut(5*doge, h.user(2))
	s, err := h.b.load()
	require.NoError(err)
	var aliceCoin utxo
	for _, u := range s.lockedUTXOs {
		if u.value == 10*doge {
			aliceCoin = u
		}
	}
	sign := func(i int, r signRequest) error {
		return h.b.cosigners[i].post("/v1/sign", r, &signResponse{})
	}
	kept := h.payoutRequest(req, []utxo{*s.pegAnchor, aliceCoin})
	require.NoError(sign(0, kept))
	require.NoError(sign(1, kept))

	c := h.signers[2]
	_, err = c.register(mallory)
	require.NoError(err)
	cs, err := c.b.load()
	require.NoError(err)
	require.True(containsOutPoint(cs.lockedUTXOs, oldCoin.outPoint), "C sees the old deposit")
	require.Equal(s.pegAnchor.outPoint, cs.pegAnchor.outPoint, "but it doesn't move C's anchor")

	_, err = h.b.registry.add(mallory)
	require.NoError(err)
	require.ErrorContains(sign(2, h.payoutRequest(req, []utxo{oldCoin})), "the oldest confirmed coin")
}

// TestSignerKeepsItsAnchorUntilItIsSpent: a signer's wallet dropping its
// anchor doesn't let it sign with another while its node's UTXO set still
// holds the one it last signed with.
func TestSignerKeepsItsAnchorUntilItIsSpent(t *testing.T) {
	require := require.New(t)
	l := &signingLog{path: t.TempDir() + "/log.json", Actions: map[string]*loggedAction{}}
	a := wire.OutPoint{Hash: chainhash.Hash{1}}
	b := wire.OutPoint{Hash: chainhash.Hash{2}}
	first := wire.NewMsgTx(1)
	first.AddTxIn(wire.NewTxIn(&a, nil, nil))
	require.NoError(l.record("payout:x", chainDogecoin, first, 100))
	next := wire.NewMsgTx(1)
	next.AddTxIn(wire.NewTxIn(&b, nil, nil))
	stillThere := func(op wire.OutPoint) bool { return op == a }
	require.ErrorContains(l.anchorMoved(chainDogecoin, next, stillThere), "still unspent")
	require.NoError(l.anchorMoved(chainDogecoin, first, stillThere))
	require.NoError(l.anchorMoved(chainDogecoin, next, func(wire.OutPoint) bool { return false }))
	require.NoError(l.anchorMoved(chainDogecoinVM, next, stillThere), "each chain has its own")
}

// TestSignedTransactionIsDeadOnlyOnChainEvidence: an earlier signed
// transaction whose inputs the wallet no longer lists still blocks a second
// while the node's UTXO set holds them; one in a block ends the action.
func TestSignedTransactionIsDeadOnlyOnChainEvidence(t *testing.T) {
	require := require.New(t)
	l := &signingLog{path: t.TempDir() + "/log.json", Actions: map[string]*loggedAction{}}
	a := wire.OutPoint{Hash: chainhash.Hash{1}}
	b := wire.OutPoint{Hash: chainhash.Hash{2}}
	first := wire.NewMsgTx(1)
	first.AddTxIn(wire.NewTxIn(&a, nil, nil))
	require.NoError(l.record("payout:x", chainDogecoin, first, 100))
	second := wire.NewMsgTx(1)
	second.AddTxIn(wire.NewTxIn(&b, nil, nil))
	none := map[wire.OutPoint]bool{}
	require.ErrorContains(l.permit("payout:x", second, none, nil, func(op wire.OutPoint) bool { return op == a }), "could confirm alongside")
	require.NoError(l.permit("payout:x", second, none, nil, func(wire.OutPoint) bool { return false }))
	inBlock := func(txid string) bool { return txid == first.TxHash().String() }
	require.ErrorContains(l.permit("payout:x", second, none, inBlock, nil), "in a block")
}

// TestContributionIsOperatingCapital: a DVMC payment to the peg address is
// never credited and owed to no one; it is the surplus, and anchors the
// first payout.
func TestContributionIsOperatingCapital(t *testing.T) {
	require := require.New(t)
	h := newBareHarness(t)
	h.contribute(3*doge, 6)
	require.Empty(h.step())
	a := h.audit()
	require.True(a.solvent())
	require.Zero(a.UnclaimedOnDoge)
	require.Equal(int64(3*doge), a.Contributed)
	require.Equal(int64(3*doge), a.Surplus)
	s, err := h.b.load()
	require.NoError(err)
	require.Empty(s.held)
	require.NotNil(s.pegAnchor)

	// Without one, a new set's first payout waits.
	bare := newBareHarness(t)
	alice := bare.user(1)
	_, err = bare.b.registry.add(alice)
	require.NoError(err)
	bare.personalDeposit(50*doge, alice, 6)
	require.NotEmpty(bare.step())
	bare.vm.mine()
	bare.pegOut(10*doge, bare.user(2))
	did, err := bare.b.step()
	require.Empty(did)
	require.ErrorContains(err, "no confirmed DOGE to anchor")
}

// TestReserveAuditDuringAPendingRelease: DogecoinVM's node still lists a
// reserve output a mempool release spends; the audit must not count it
// beside the release's change (V8), before, during or after the release.
func TestReserveAuditDuringAPendingRelease(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	h.vm.listSpentInMempool = true // as btcd's gettxout does
	alice := h.user(1)
	h.deposit(100*doge, &alice, 6)
	before := h.audit()
	require.Zero(before.Circulating)
	require.NotEmpty(h.step()) // the release, in the mempool
	during := h.audit()
	require.Equal(int64(100*doge), during.Circulating, "%+v", during)
	require.True(during.solvent())
	h.vm.mine()
	after := h.audit()
	require.Equal(int64(100*doge), after.Circulating)
	require.True(after.solvent())
}

// TestMainnetConfirmationFloor: on mainnet no deposit is credited at fewer
// than six confirmations, whatever the tiers say.
func TestMainnetConfirmationFloor(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	h.b.confirmationTiers = []confirmationTier{{UpTo: 10 * doge, Confirmations: 1}}
	require.Equal(int64(1), h.b.confirmationsFor(5*doge))
	h.b.confFloor = minMainnetConfirmations
	require.Equal(int64(6), h.b.confirmationsFor(5*doge))
	require.Equal(int64(6), h.b.confirmationsFor(50*doge), "the full -confirmations")
	alice := h.user(1)
	h.deposit(5*doge, &alice, 1)
	require.Empty(h.step())
}

// TestShallowCreditIsCapped: deposits each small enough for the lowest
// tier are credited early only up to the largest tier's amount in total.
func TestShallowCreditIsCapped(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	h.b.confirmationTiers = []confirmationTier{{UpTo: 2 * doge, Confirmations: 1}}
	alice := h.user(1)
	for i := 0; i < 3; i++ {
		h.deposit(2*doge, &alice, 1)
	}
	credited := 0
	for h.step() != "" {
		h.vm.mine()
		credited++
	}
	require.Equal(1, credited)
	for i := 0; i < 5; i++ {
		h.doge.mine()
	}
	for h.step() != "" {
		h.vm.mine()
		credited++
	}
	require.Equal(3, credited)
}

// TestForcedRefundThroughSigners: a deposit waiting under the circulating
// cap is refunded through signers only with "force", never by one that
// signed a release of it, and a signer that signed the refund never
// credits it.
func TestForcedRefundThroughSigners(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.b.maxCirculating = 1 * doge
	for _, c := range h.signers {
		c.b.maxCirculating = 1 * doge
	}
	alice, back := h.user(1), h.user(9)
	dep := h.deposit(5*doge, &alice, 6)
	require.Empty(h.step())
	op := wire.OutPoint{Hash: dep.TxHash()}
	backAddr, err := back.address(h.b.dogeParams)
	require.NoError(err)
	approve := func(c *cosigner, extra string) {
		path := t.TempDir() + "/approvals"
		require.NoError(os.WriteFile(path, []byte(op.String()+" "+backAddr.EncodeAddress()+extra+"\n"), 0o600))
		c.refundApprovals = path
	}
	approve(h.signers[0], "")
	approve(h.signers[1], "")
	_, err = h.b.refund(op, back, true)
	require.ErrorContains(err, "not held for a refund")

	h.signers[1].log.Actions[actionRelease+":"+op.String()] = &loggedAction{Txs: []loggedTx{{Txid: "00"}}}
	for _, c := range h.signers {
		approve(c, " force")
	}
	_, err = h.b.refund(op, back, true) // signers 0 and 2 sign it
	require.NoError(err)
	refund := h.doge.txs[len(h.doge.txs)-1].tx
	_, _, _, err = h.signers[1].check(signRequest{Chain: chainDogecoin, Action: refundAction(op, back), Tx: encodeTx(refund)})
	require.Error(err)

	require.True(h.signers[0].log.has(actionRefund + ":" + op.String()))
	// Kept back rather than sent: signer 0's log alone still stops it
	// crediting the deposit.
	h.doge.txs = h.doge.txs[:len(h.doge.txs)-1]
	s, err := h.b.load()
	require.NoError(err)
	d := deposit{outPoint: op, value: 5 * doge, dest: alice, valid: true, confirmations: 6}
	tx := h.b.buildRelease([]utxo{*s.reserveAnchor}, s.reserveAnchor.value, d)
	release := signRequest{Chain: chainDogecoinVM, Tx: encodeTx(tx), Action: action{Kind: actionRelease, Deposit: op.String()}}
	_, _, _, err = h.signers[0].check(release)
	require.ErrorContains(err, "never credits it")
}

// TestStuckPaymentAndHeldDepositAlert: a payout unconfirmed long after it
// was sent fails the bridge check, and so does a held deposit worth
// refunding (dust isn't alerted on).
func TestStuckPaymentAndHeldDepositAlert(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice := h.user(1)
	h.deposit(10*doge, &alice, 6)
	for h.step() != "" {
		h.vm.mine()
	}
	h.pegOut(5*doge, h.user(2))
	require.NotEmpty(h.step())
	hc := &healthChecker{b: h.b, stallBlocks: 10}
	s, err := h.b.load()
	require.NoError(err)
	require.True(hc.bridgeLiveness(s).OK, "just sent")
	h.doge.txs[len(h.doge.txs)-1].time = time.Now().Add(-time.Hour).Unix()
	s, err = h.b.load()
	require.NoError(err)
	c := hc.bridgeLiveness(s)
	require.False(c.OK)
	require.Contains(c.Detail, "unconfirmed")

	h.doge.mine()
	h.deposit(doge/2, &alice, 20) // below the minimum: dust
	s, err = h.b.load()
	require.NoError(err)
	require.True(hc.bridgeLiveness(s).OK)
	h.deposit(3*doge, nil, 20) // no destination: worth refunding
	s, err = h.b.load()
	require.NoError(err)
	c = hc.bridgeLiveness(s)
	require.False(c.OK)
	require.Contains(c.Detail, "held and owed back")
}

// TestWalletHistoryIsNeverTruncated: past the cap the read fails rather
// than drop old payouts, which would then be paid again.
func TestWalletHistoryIsNeverTruncated(t *testing.T) {
	require.NoError(t, historyComplete(maxWalletEntries-1))
	require.ErrorContains(t, historyComplete(maxWalletEntries), "stops rather than miss old payouts")
}
