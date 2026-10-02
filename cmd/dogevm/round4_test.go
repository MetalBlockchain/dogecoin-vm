package main

import (
	"path/filepath"
	"strings"
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

// rebuildLogs is the cutover after the signers' logs were lost (or never
// kept, as on the live set): each signer starts a new log and adopts what
// the chains show it signed, which sets its anchors and so marks both
// chains as needing a fence.
func (h *cosignHarness) rebuildLogs() {
	h.t.Helper()
	for _, c := range h.signers {
		c.log = &signingLog{path: filepath.Join(h.t.TempDir(), "signing-log.json"), Actions: map[string]*loggedAction{}}
		s, err := c.b.load()
		require.NoError(h.t, err)
		_, err = c.adopt(s)
		require.NoError(h.t, err)
		require.True(h.t, c.log.Unfenced[chainDogecoin] && c.log.Unfenced[chainDogecoinVM])
	}
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

// TestFenceKillsKeptBackSignatures (round 4, T5): the live signers signed
// without logs or an anchor line, so a payout and a release signed then
// and kept back can spend any coins, off today's line. Logs rebuilt from
// the chains can't show them. Until a fence has spent every coin on a
// chain, no signer signs anything else there; once it is deep enough, the
// kept-back transactions can never confirm, and the bridge carries on.
// Without the fence they would: their coins are ones the line never spends.
func TestFenceKillsKeptBackSignatures(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice, bob := h.user(1), h.user(2)
	capital := h.contribute(10*doge, 50) // operating capital for the fences' fees
	h.deposit(10*doge, &alice, 9)
	for h.step() != "" {
		h.vm.mine()
	}
	req := h.pegOut(5*doge, bob)
	h.vm.mine()

	// Kept back from the old days: bob's payout from the capital coin,
	// and a credit of a second deposit from the coin bob paid into the
	// reserve. Neither is on today's lines.
	s, err := h.b.load()
	require.NoError(err)
	p, ok := findPegOut(s.pegOuts, req.TxHash())
	require.True(ok)
	capitalCoin := wire.OutPoint{Hash: capital.TxHash()}
	require.NotEqual(capitalCoin, s.pegAnchor.outPoint)
	payout := wire.NewMsgTx(1)
	payout.AddTxIn(wire.NewTxIn(&capitalCoin, nil, nil))
	payout.AddTxOut(wire.NewTxOut(p.value-h.b.dogeFee, bob.pkScript()))
	payout.AddTxOut(wire.NewTxOut(10*doge-p.value, h.b.signers.pkScript()))
	payout.AddTxOut(nullData(encodePayment(p.txid)))
	payout = h.oldSignature(payout, [][]byte{h.b.signers.redeemScript})

	dep2 := h.deposit(4*doge, &alice, 9)
	reserveCoin := wire.OutPoint{Hash: req.TxHash()}
	require.NotEqual(reserveCoin, s.reserveAnchor.outPoint)
	release := wire.NewMsgTx(wire.TxVersion)
	release.AddTxIn(wire.NewTxIn(&reserveCoin, nil, nil))
	release.AddTxOut(wire.NewTxOut(4*doge-h.b.vmFee, alice.pkScript()))
	release.AddTxOut(wire.NewTxOut(doge, h.b.signers.pkScript()))
	release.AddTxOut(nullData(encodeRelease(wire.OutPoint{Hash: dep2.TxHash()})))
	release = h.oldSignature(release, [][]byte{h.b.signers.redeemScript})

	h.rebuildLogs()
	_, err = h.b.step()
	require.ErrorContains(err, "signs only a fence", "nothing but a fence until the cutover")

	_, err = h.b.fence(chainDogecoin)
	require.NoError(err)
	_, err = h.b.fence(chainDogecoinVM)
	require.NoError(err)
	h.vm.mine()
	h.doge.mine()
	cs, err := h.signers[2].b.load()
	require.NoError(err)
	require.ErrorContains(h.signers[2].fenced(cs, chainDogecoin), "confirmations yet", "Dogecoin's fence must be deep first")
	require.NoError(h.signers[2].fenced(cs, chainDogecoinVM), "DogecoinVM's is final in its block")
	for range 6 {
		h.doge.mine()
	}

	_, err = h.doge.send(payout)
	require.ErrorContains(err, "missing or spent", "the kept-back payout's coin is spent")
	_, err = h.vm.send(release)
	require.ErrorContains(err, "missing or spent", "the kept-back release's coin is spent")

	// The bridge carries on: the deposit is credited, the peg-out paid.
	did := ""
	for range 6 {
		got, err := h.b.step()
		require.NoError(err)
		did += got
		h.vm.mine()
		h.doge.mine()
	}
	require.Contains(did, "paid")
	for _, c := range h.signers { // each clears its mark when next asked
		cs, err := c.b.load()
		require.NoError(err)
		require.NoError(c.fenced(cs, chainDogecoin))
		require.NoError(c.fenced(cs, chainDogecoinVM))
		require.Empty(c.log.Unfenced)
	}
	a := h.audit()
	require.True(a.solvent(), "%+v", a)

	// A log rebuilt again later adopts the fences (and any cancellation)
	// it signed, by their tags, and the log loads again.
	c := h.signers[0]
	c.log = &signingLog{path: filepath.Join(t.TempDir(), "signing-log.json"), Actions: map[string]*loggedAction{}}
	cs, err = c.b.load()
	require.NoError(err)
	adopted, err := c.adopt(cs)
	require.NoError(err)
	require.NotEmpty(adopted)
	fences := 0
	for key := range c.log.Actions {
		if strings.HasPrefix(key, actionFence+":") {
			fences++
		}
	}
	require.Equal(2, fences, "both fences, one per chain")
	_, err = openSigningLog(c.log.path)
	require.NoError(err)
}

// TestFenceSpendsEveryCoinTheSignerSees (round 4, T5): a fence that leaves
// out a coin the signer holds could leave a kept-back signature alive, so
// the signer refuses it; and a fence is only for a log that needs one.
func TestFenceSpendsEveryCoinTheSignerSees(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice := h.user(1)
	h.contribute(10*doge, 50)
	h.deposit(10*doge, &alice, 9)
	for h.step() != "" {
		h.vm.mine()
	}
	c := h.signers[2]
	s, err := h.b.load()
	require.NoError(err)
	inputs, err := s.fenceInputs(chainDogecoin)
	require.NoError(err)
	require.Greater(len(inputs), 2)
	request := func(in []utxo) signRequest {
		tx, _, err := h.b.buildFence(chainDogecoin, in)
		require.NoError(err)
		return signRequest{Chain: chainDogecoin, Tx: encodeTx(tx), Action: action{Kind: actionFence, Deposit: in[0].outPoint.String()}}
	}
	_, _, _, err = c.check(request(inputs))
	require.ErrorContains(err, "needs no fence", "a log that was never rebuilt signs no fence")

	h.rebuildLogs()
	_, _, _, err = c.check(request(inputs[:len(inputs)-1]))
	require.ErrorContains(err, "not the transaction this signer would build")
	_, _, _, err = c.check(request(inputs))
	require.NoError(err)
}

// TestKeptBackPayoutSentBeforeTheFence (round 4, T5): if the coordinator
// sends a kept-back payout after the logs were rebuilt and before the
// fence, the signers that signed it see a signature their new log lacks and
// quarantine themselves: the fence can't be signed, and nothing else is.
func TestKeptBackPayoutSentBeforeTheFence(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice, bob := h.user(1), h.user(2)
	h.contribute(10*doge, 50)
	h.deposit(10*doge, &alice, 9)
	for h.step() != "" {
		h.vm.mine()
	}
	req := h.pegOut(5*doge, bob)
	payout, redeems, sigs := h.keptPayout(req)
	h.rebuildLogs()
	require.NoError(h.b.signers.assemble(payout, redeems, sigs))
	_, err := h.doge.send(payout)
	require.NoError(err)
	h.doge.mine()

	_, err = h.b.fence(chainDogecoin)
	require.ErrorContains(err, "quarantined")
	require.NotEmpty(h.signers[0].quarantined())
	require.NotEmpty(h.signers[1].quarantined())
}

// TestAFenceSomeoneElseMadeDoesntCount (round 4, T5): anyone can pay the
// peg address with a fence's tag. Only a fence that spends the set's own
// coins, from the rebuilt log's anchor on, ends the cutover.
func TestAFenceSomeoneElseMadeDoesntCount(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice := h.user(1)
	h.contribute(10*doge, 50)
	h.deposit(10*doge, &alice, 9)
	for h.step() != "" {
		h.vm.mine()
	}
	h.rebuildLogs()
	c := h.signers[2]
	s, err := c.b.load()
	require.NoError(err)
	tip := s.pegAnchor.outPoint

	// A stranger's payment to the peg tagged as a fence, naming the tip,
	// deep in the chain, with nothing older left unspent but the set's own.
	fake := wire.NewMsgTx(1)
	fake.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
	fake.AddTxOut(wire.NewTxOut(doge, h.b.signers.pkScript()))
	fake.AddTxOut(nullData(encodeFence(tip)))
	h.doge.add(fake, 1)
	for range 10 {
		h.doge.mine()
	}
	s, err = c.b.load()
	require.NoError(err)
	require.ErrorContains(c.fenced(s, chainDogecoin), "no fence on dogecoin from this log's anchor")
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

// TestRefenceRecoversABrokenAnchor (round 4): a log whose anchor no longer
// leads to the tip (here the line ended at a spend that paid nothing back)
// refuses anchors -set; with -refence its anchor moves to the tip and it
// needs a cutover fence before it signs anything else, so whatever it
// signed from the old anchor can't confirm.
func TestRefenceRecoversABrokenAnchor(t *testing.T) {
	require := require.New(t)
	peg, other := []byte{0xa9, 0x14, 0xaa}, []byte{0x51}
	at := func(tx *wire.MsgTx, i uint32) wire.OutPoint { return wire.OutPoint{Hash: tx.TxHash(), Index: i} }
	seed := lineTx(nil, 1, peg)
	p1 := lineTx(&wire.OutPoint{Hash: seed.TxHash()}, 0, other, peg)
	dead := lineTx(&wire.OutPoint{Hash: p1.TxHash(), Index: 1}, 0, other)
	later := lineTx(nil, 3, peg)
	s := &pegState{pegLine: newLineage([]chainTx{{tx: seed, confirmations: 20}, {tx: p1, confirmations: 15},
		{tx: dead, confirmations: 12}, {tx: later, confirmations: 10}}, peg, 6)}
	c := &cosigner{log: &signingLog{path: filepath.Join(t.TempDir(), "signing-log.json"), Actions: map[string]*loggedAction{},
		Anchors: map[string]string{chainDogecoin: at(p1, 1).String()}}}
	_, err := c.fillAnchors(s, false)
	require.ErrorIs(err, errLineEnded)
	require.Equal(at(p1, 1).String(), c.log.Anchors[chainDogecoin], "unchanged")
	require.Empty(c.log.Unfenced)

	_, err = c.fillAnchors(s, true)
	require.NoError(err)
	require.Equal(at(later, 0).String(), c.log.Anchors[chainDogecoin])
	require.True(c.log.Unfenced[chainDogecoin], "a fence first")
}

// TestFenceGateCoversBothChains: while a rebuilt log is unfenced on either
// chain, it signs nothing but fences on both. A release on a fenced
// DogecoinVM could otherwise confirm beside a refund or payout of the same
// deposit, signed before and kept back, on a still unfenced Dogecoin.
func TestFenceGateCoversBothChains(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice := h.user(1)
	h.contribute(10*doge, 50)
	h.deposit(10*doge, &alice, 9)
	for h.step() != "" {
		h.vm.mine()
	}
	dep := h.deposit(4*doge, &alice, 9)
	h.rebuildLogs()
	_, err := h.b.fence(chainDogecoinVM) // only DogecoinVM
	require.NoError(err)
	h.vm.mine()

	c := h.signers[2]
	s, err := c.b.load()
	require.NoError(err)
	require.NoError(c.fenced(s, chainDogecoinVM), "DogecoinVM's cutover is done")
	d, ok := findDeposit(s.deposits, wire.OutPoint{Hash: dep.TxHash()})
	require.True(ok)
	inputs, total, err := s.releaseInputs(d.value)
	require.NoError(err)
	tx := h.b.buildRelease(inputs, total, d)
	req := signRequest{Chain: chainDogecoinVM, Tx: encodeTx(tx), Register: []string{encodeDest(alice)},
		Action: action{Kind: actionRelease, Deposit: d.outPoint.String()}}
	_, _, _, err = c.check(req)
	require.ErrorContains(err, "signs only a fence on dogecoin", "a DogecoinVM release waits for Dogecoin's fence")
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
