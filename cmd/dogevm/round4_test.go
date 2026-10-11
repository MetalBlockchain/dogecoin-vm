package main

import (
	"testing"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
	"github.com/stretchr/testify/require"
)

// lineTx is a transaction for the lineage tests: from (or a unique coin
// elsewhere), paying 1000 to each script in outs.
func lineTx(from *wire.OutPoint, salt byte, outs ...[]byte) *wire.MsgTx {
	tx := wire.NewMsgTx(1)
	if from == nil {
		from = &wire.OutPoint{Hash: chainhash.Hash{salt, 0xee}}
	}
	tx.AddTxIn(wire.NewTxIn(from, nil, nil))
	for _, o := range outs {
		tx.AddTxOut(wire.NewTxOut(1000, o))
	}
	return tx
}

// TestRootIsTheOldestCoinTheSignersSpent (round 4, T12): once the signers
// have spent a coin at the script, a coin in an older block that nobody has
// spent (shown to a wallet late, or never touched) can't become the root:
// the line, and so every signer's anchor, stays where it is. Under the old
// rule (the oldest coin ever) it took the line over.
func TestRootIsTheOldestCoinTheSignersSpent(t *testing.T) {
	require := require.New(t)
	peg, other := []byte{0xa9, 0x14, 0xaa}, []byte{0x51}
	at := func(tx *wire.MsgTx, i uint32) wire.OutPoint { return wire.OutPoint{Hash: tx.TxHash(), Index: i} }
	seed := lineTx(nil, 1, peg)
	p1 := lineTx(&wire.OutPoint{Hash: seed.TxHash()}, 0, other, peg)
	txs := []chainTx{{tx: seed, confirmations: 20}, {tx: p1, confirmations: 10}}
	tip, root, _, ok, err := newLineage(txs, peg, 6).tip()
	require.NoError(err)
	require.True(ok)
	require.Equal(at(seed, 0), root)
	require.Equal(at(p1, 1), tip)

	late := lineTx(nil, 2, peg) // older than the seed, never spent
	tip, root, _, ok, err = newLineage(append(txs, chainTx{tx: late, confirmations: 30}), peg, 6).tip()
	require.NoError(err)
	require.True(ok)
	require.Equal(at(seed, 0), root, "a spent root wins over an older coin nobody spent")
	require.Equal(at(p1, 1), tip)

	// Before anything is spent, the oldest deep coin is the root.
	_, root, _, _, err = newLineage([]chainTx{{tx: seed, confirmations: 20}, {tx: late, confirmations: 30}}, peg, 6).tip()
	require.NoError(err)
	require.Equal(at(late, 0), root)
}

// TestLogDoesntFollowARestart (round 4, T12): the line read from history
// starts again after a confirmed spend that paid nothing back, but a
// signer's log doesn't follow it: the coin its last signature spent led
// nowhere, and which coin comes next is an operator's decision (a fence),
// not a signer's.
func TestLogDoesntFollowARestart(t *testing.T) {
	require := require.New(t)
	peg, other := []byte{0xa9, 0x14, 0xaa}, []byte{0x51}
	at := func(tx *wire.MsgTx, i uint32) wire.OutPoint { return wire.OutPoint{Hash: tx.TxHash(), Index: i} }
	seed := lineTx(nil, 1, peg)
	p1 := lineTx(&wire.OutPoint{Hash: seed.TxHash()}, 0, other, peg)
	dead := lineTx(&wire.OutPoint{Hash: p1.TxHash(), Index: 1}, 0, other)
	later := lineTx(nil, 3, peg)
	line := newLineage([]chainTx{{tx: seed, confirmations: 20}, {tx: p1, confirmations: 15},
		{tx: dead, confirmations: 12}, {tx: later, confirmations: 10}}, peg, 6)
	tip, _, _, ok, err := line.tip()
	require.NoError(err)
	require.True(ok)
	require.Equal(at(later, 0), tip, "history restarts the line")

	log := &signingLog{Actions: map[string]*loggedAction{}, Anchors: map[string]string{chainDogecoin: at(p1, 1).String()}}
	probe := wire.NewMsgTx(1)
	tipOp := at(later, 0)
	probe.AddTxIn(wire.NewTxIn(&tipOp, nil, nil))
	err = log.anchorMoved(chainDogecoin, probe, nil, line)
	require.ErrorIs(err, errLineEnded)
}

// TestCancelUsesMoreReserveWhenTheAnchorIsTiny (round 4, T8): a release can
// leave the reserve's anchor with little more than pegDust. A cancellation
// then spends the anchor first and as much more of the reserve as it needs
// to pay its fee and leave pegDust of change; an input it doesn't need is
// refused. Before, it spent the anchor alone and failed.
func TestCancelUsesMoreReserveWhenTheAnchorIsTiny(t *testing.T) {
	require := require.New(t)
	b := &bridge{vmFee: doge / 10}
	set, err := newSignerSet(2, 3)
	require.NoError(err)
	b.signers = set
	script := b.signers.pkScript()
	tiny := utxo{outPoint: wire.OutPoint{Hash: chainhash.Hash{1}}, value: pegDust, pkScript: script, confirmations: 3}
	big := utxo{outPoint: wire.OutPoint{Hash: chainhash.Hash{2}}, value: 100 * doge, pkScript: script, confirmations: 5}
	other := utxo{outPoint: wire.OutPoint{Hash: chainhash.Hash{3}}, value: 50 * doge, pkScript: script, confirmations: 4}
	s := &pegState{reserveAnchor: &tiny, reserveUTXOs: []utxo{tiny, big, other}}
	op := wire.OutPoint{Hash: chainhash.Hash{9}}

	inputs, err := s.cancelInputs(b.vmFee)
	require.NoError(err)
	require.Equal([]utxo{tiny, big}, inputs, "the anchor first, then the largest other coin")
	tx, err := b.buildCancel(inputs, op)
	require.NoError(err)
	require.Equal(tiny.outPoint, tx.TxIn[0].PreviousOutPoint)
	require.Equal(tiny.value+big.value-b.vmFee, tx.TxOut[0].Value)
	require.True(tx.TxOut[0].Value >= pegDust)

	_, err = b.buildCancel([]utxo{tiny, big, other}, op)
	require.ErrorContains(err, "does not need")
	_, err = b.buildCancel([]utxo{tiny}, op)
	require.ErrorContains(err, "needs")
}

// TestSignerOfAWithheldReleaseMayCancel (Grok r4 #2, T11): A and B sign a
// release and B keeps it back. A, which signed it, may still sign the
// cancellation with C: both spend the reserve's anchor, so at most one of
// them confirms. (Before, A refused, and with B unwilling the honest
// signers couldn't cancel at all.) The refund still waits until the
// cancellation is in a block and A's release is dead.
func TestSignerOfAWithheldReleaseMayCancel(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice, back := h.user(1), h.user(9)
	dep := h.deposit(5*doge, &alice, 6)
	op := wire.OutPoint{Hash: dep.TxHash()}
	s, err := h.b.load()
	require.NoError(err)
	d, ok := findDeposit(s.deposits, op)
	require.True(ok)
	inputs, total, err := s.releaseInputs(d.value)
	require.NoError(err)
	release := h.b.buildRelease(inputs, total, d)
	redeems := make([][]byte, len(inputs))
	for i := range redeems {
		redeems[i] = h.b.signers.redeemScript
	}
	kept := h.signWith(&proposal{Chain: chainDogecoinVM, tx: release, redeems: redeems,
		Action: action{Kind: actionRelease, Deposit: op.String()}, Register: []destination{alice}}, 0, 1)

	h.approveRefunds(op, back, " force")
	cancelInputs, err := s.cancelInputs(h.b.vmFee)
	require.NoError(err)
	cancelTx, err := h.b.buildCancel(cancelInputs, op)
	require.NoError(err)
	cancelRedeems := make([][]byte, len(cancelInputs))
	for i := range cancelRedeems {
		cancelRedeems[i] = h.b.signers.redeemScript
	}
	cancelSigs := h.signWith(&proposal{Chain: chainDogecoinVM, tx: cancelTx, redeems: cancelRedeems,
		Action: action{Kind: actionCancel, Deposit: op.String()}}, 0, 2) // A and C
	require.NoError(h.b.signers.assemble(cancelTx, cancelRedeems, cancelSigs))
	_, err = h.vm.send(cancelTx)
	require.NoError(err)
	h.vm.mine()

	require.NoError(h.b.signers.assemble(release, redeems, kept))
	_, err = h.vm.send(release)
	require.ErrorContains(err, "missing or spent", "the kept-back release can't confirm after the cancellation")
	_, _, _, err = h.signers[0].check(h.refundRequest(op, d.value, back))
	require.NoError(err, "A's release is dead: A refunds")
}

// keptPayout has signers 0 and 1 sign peg-out req's payout, and returns it
// with its signatures, unsent: what a coordinator could keep back.
func (h *cosignHarness) keptPayout(req *wire.MsgTx) (*wire.MsgTx, [][]byte, map[int][][]byte) {
	h.t.Helper()
	s, err := h.b.load()
	require.NoError(h.t, err)
	p, ok := findPegOut(s.pegOuts, req.TxHash())
	require.True(h.t, ok)
	var confirmed []utxo
	for _, u := range s.lockedUTXOs {
		if u.confirmations > 0 {
			confirmed = append(confirmed, u)
		}
	}
	inputs, err := s.payoutInputs(confirmed, p.value)
	require.NoError(h.t, err)
	tx, err := h.b.buildPayout(inputs, p.value, p.dest, encodePayment(p.txid), h.b.dogeFee)
	require.NoError(h.t, err)
	redeems := make([][]byte, len(inputs))
	for i, u := range inputs {
		redeems[i] = s.redeemFor[string(u.pkScript)]
	}
	sigs := h.signWith(&proposal{Chain: chainDogecoin, tx: tx, redeems: redeems,
		Action: action{Kind: actionPayout, PegOut: p.txid.String()}}, 0, 1)
	return tx, redeems, sigs
}

// oldSignature is tx signed by signers 0 and 1 as the live set's binary
// signed before signers kept logs or followed an anchor line: any coins,
// no checks. It is assembled, ready for a coordinator to send any time.
func (h *cosignHarness) oldSignature(tx *wire.MsgTx, redeems [][]byte) *wire.MsgTx {
	h.t.Helper()
	sigs := map[int][][]byte{}
	for _, i := range []int{0, 1} {
		s, err := signInputs(tx, redeems, h.signers[i].key)
		require.NoError(h.t, err)
		sigs[h.b.signers.indexOf(h.signers[i].key.PubKey())] = s
	}
	require.NoError(h.t, h.b.signers.assemble(tx, redeems, sigs))
	return tx
}

// TestWalkedTips (round 4, T7): signer-log anchors reports where each
// anchor leads along confirmed spends, which the upgrade compares with
// the coordinator's tips: an anchor a confirmed step behind leads to the
// tip; one whose line ended leads nowhere.
func TestWalkedTips(t *testing.T) {
	require := require.New(t)
	peg, other := []byte{0xa9, 0x14, 0xaa}, []byte{0x51}
	at := func(tx *wire.MsgTx, i uint32) wire.OutPoint { return wire.OutPoint{Hash: tx.TxHash(), Index: i} }
	seed := lineTx(nil, 1, peg)
	p1 := lineTx(&wire.OutPoint{Hash: seed.TxHash()}, 0, other, peg)
	s := &pegState{pegLine: newLineage([]chainTx{{tx: seed, confirmations: 20}, {tx: p1, confirmations: 10}}, peg, 6)}
	log := &signingLog{Anchors: map[string]string{chainDogecoin: at(seed, 0).String()}}
	require.Equal(at(p1, 1).String(), walkedTips(log, s)[chainDogecoin])

	dead := lineTx(&wire.OutPoint{Hash: p1.TxHash(), Index: 1}, 0, other)
	s.pegLine = newLineage([]chainTx{{tx: seed, confirmations: 20}, {tx: p1, confirmations: 10}, {tx: dead, confirmations: 5}}, peg, 6)
	require.Contains(walkedTips(log, s)[chainDogecoin], "error: the line ended")
}

// TestTagsAreDistinct: every OP_RETURN tag the bridge writes or reads is
// different, so no transaction reads as another kind.
func TestTagsAreDistinct(t *testing.T) {
	tags := map[string][]byte{"deposit": tagDeposit, "release": tagRelease, "pegOut": tagPegOut, "payment": tagPayment,
		"refund": tagRefund, "contrib": tagContrib, "cancel": tagCancel, "fence": tagFence}
	seen := map[string]string{}
	for name, tag := range tags {
		require.Len(t, tag, 4, name)
		if other, dup := seen[string(tag)]; dup {
			t.Fatalf("%s and %s share the tag %q", name, other, tag)
		}
		seen[string(tag)] = name
	}
}
