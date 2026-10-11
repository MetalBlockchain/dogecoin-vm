package main

// Tests for the third review's findings (hardening-review-r3): two quorums
// that share a dishonest signer, chain evidence by signed IDs, and logs
// that load again.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// signWith has the signers at idx sign p, as the coordinator would ask
// them, and returns their signatures by position in the set.
func (h *cosignHarness) signWith(p *proposal, idx ...int) map[int][][]byte {
	h.t.Helper()
	sigs := map[int][][]byte{}
	for _, i := range idx {
		at, s, err := h.b.cosigners[i].sign(p, h.b.signers)
		require.NoError(h.t, err, "signer %d", i)
		sigs[at] = s
	}
	return sigs
}

// dishonest makes the coordinator hold signer i's key: it signs whatever
// the coordinator asks, without its checks or its log.
func (h *cosignHarness) dishonest(i int) {
	h.b.signers.privKeys = []*btcec.PrivateKey{h.signers[i].key}
}

// approveRefunds writes an approvals file listing op's refund to dest for
// every signer, with extra (" force") after it.
func (h *cosignHarness) approveRefunds(op wire.OutPoint, dest destination, extra string) {
	h.t.Helper()
	addr, err := dest.address(h.b.dogeParams)
	require.NoError(h.t, err)
	for _, c := range h.signers {
		path := filepath.Join(h.t.TempDir(), "approvals")
		require.NoError(h.t, os.WriteFile(path, []byte(op.String()+" "+addr.EncodeAddress()+extra+"\n"), 0o600))
		c.refundApprovals = path
	}
}

// setPolicy applies edit to the coordinator's policy and every signer's.
func (h *cosignHarness) setPolicy(edit func(b *bridge)) {
	edit(h.b)
	for _, c := range h.signers {
		edit(c.b)
	}
}

// refundRequest is the request for refunding deposit op of value to dest
// from the peg's anchor, as the coordinator would build it now.
func (h *cosignHarness) refundRequest(op wire.OutPoint, value int64, dest destination) signRequest {
	h.t.Helper()
	s, err := h.b.load()
	require.NoError(h.t, err)
	var confirmed []utxo
	for _, u := range s.lockedUTXOs {
		if u.confirmations > 0 {
			confirmed = append(confirmed, u)
		}
	}
	inputs, err := s.payoutInputs(confirmed, value)
	require.NoError(h.t, err)
	tx, err := h.b.buildPayout(inputs, value, dest, encodeRefund(op), h.b.dogeFee)
	require.NoError(h.t, err)
	return signRequest{Chain: chainDogecoin, Action: refundAction(op, dest), Tx: encodeTx(tx)}
}

// TestPolicyChangeDoesntSkipTheCancellation (Codex r3 #2): A and B sign a
// release of a deposit and B keeps it back. A policy change makes the
// deposit look held. B and C must not refund it on Dogecoin while that
// release could still confirm on DogecoinVM: C refuses a refund until the
// deposit's cancellation, which spends the reserve's anchor the release
// spends too, is in a block. A, which signed the release, signs the
// refund once that release is provably dead.
func TestPolicyChangeDoesntSkipTheCancellation(t *testing.T) {
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
	p := &proposal{Chain: chainDogecoinVM, tx: release, redeems: redeems,
		Action: action{Kind: actionRelease, Deposit: op.String()}, Register: []destination{alice}}
	kept := h.signWith(p, 0, 1) // kept back by B and the coordinator

	h.setPolicy(func(b *bridge) { b.minDeposit = 6 * doge }) // now it looks held
	h.approveRefunds(op, back, "")
	direct := h.refundRequest(op, d.value, back)
	_, _, _, err = h.signers[2].check(direct)
	require.ErrorContains(err, "names a destination", "no refund without a confirmed cancellation")
	_, _, _, err = h.signers[0].check(direct)
	require.ErrorContains(err, "signed a release", "A signed a release that could still confirm")

	// The coordinator and B (dishonest) go through C for the cancellation.
	h.dishonest(1)
	h.servers[0].Close()
	_, err = h.b.refund(op, back, false)
	require.ErrorIs(err, errCancelSent)
	h.vm.mine()

	// The kept-back release can never confirm now.
	require.NoError(h.b.signers.assemble(release, redeems, kept))
	_, err = h.vm.send(release)
	require.ErrorContains(err, "missing or spent")

	// A's release is dead on the chain, so A may refund it too.
	_, _, _, err = h.signers[0].check(h.refundRequest(op, d.value, back))
	require.NoError(err)
	_, err = h.b.refund(op, back, false)
	require.NoError(err)
	require.Equal(d.value-h.b.dogeFee, paidTo(h.doge, back))
}

// TestFeePromisesFitTheSurplusAcrossQuorums (Codex r3 #3): each honest
// signer promises at most half the surplus (2 of 3: n-t+1 = 2 honest
// signers), so two quorums sharing a dishonest signer never promise more
// than the surplus between them.
func TestFeePromisesFitTheSurplusAcrossQuorums(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.setPolicy(func(b *bridge) { b.vmFee = 4 * doge / 10 }) // surplus: the 1 DOGE seed
	alice := h.user(1)
	var ops []wire.OutPoint
	for range 2 {
		dep := h.deposit(3*doge, &alice, 6)
		ops = append(ops, wire.OutPoint{Hash: dep.TxHash()})
	}
	h.setPolicy(func(b *bridge) { b.minDeposit = 4 * doge }) // held, with a destination
	for _, c := range h.signers {
		path := filepath.Join(t.TempDir(), "approvals")
		addr, err := h.user(9).address(h.b.dogeParams)
		require.NoError(err)
		require.NoError(os.WriteFile(path, []byte(ops[0].String()+" "+addr.EncodeAddress()+"\n"+ops[1].String()+" "+addr.EncodeAddress()+"\n"), 0o600))
		c.refundApprovals = path
	}
	cancel := func(op wire.OutPoint) signRequest {
		s, err := h.b.load()
		require.NoError(err)
		inputs, err := s.cancelInputs(h.b.vmFee)
		require.NoError(err)
		tx, err := h.b.buildCancel(inputs, op)
		require.NoError(err)
		return signRequest{Chain: chainDogecoinVM, Action: action{Kind: actionCancel, Deposit: op.String()}, Tx: encodeTx(tx)}
	}
	sign := func(i int, r signRequest) error { return h.b.cosigners[i].post("/v1/sign", r, &signResponse{}) }

	require.NoError(sign(0, cancel(ops[0])), "A: 0.4 of 1 DOGE, within half")
	require.NoError(sign(1, cancel(ops[0])))
	// Kept back. A second fee from A would make 0.8: more than half.
	require.ErrorContains(sign(0, cancel(ops[1])), "1/2 of the peg's surplus")
	// C, which promised nothing, may: A's 0.4 and C's 0.4 fit in the 1.
	require.NoError(sign(2, cancel(ops[1])))
}

// TestSignerWithNoAnchorSignsNothing (Grok r3 N3): a log with no anchor
// for a chain (an old adopted one) signs nothing there until its operator
// sets it to the line's tip; adopt sets it.
func TestSignerWithNoAnchorSignsNothing(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice := h.user(1)
	h.deposit(10*doge, &alice, 6)
	require.NotEmpty(h.step()) // A and B: the reserve's line moves
	h.vm.mine()
	second := h.deposit(10*doge, &alice, 6)
	c := h.signers[2]
	c.log.Anchors = nil
	s, err := h.b.load()
	require.NoError(err)
	d, ok := findDeposit(s.deposits, wire.OutPoint{Hash: second.TxHash()})
	require.True(ok)
	tx := h.b.buildRelease([]utxo{*s.reserveAnchor}, s.reserveAnchor.value, d)
	req := signRequest{Chain: chainDogecoinVM, Tx: encodeTx(tx), Register: []string{encodeDest(alice)},
		Action: action{Kind: actionRelease, Deposit: d.outPoint.String()}}
	_, _, _, err = c.check(req)
	require.ErrorContains(err, "no dogecoinvm anchor")

	// adopt (as for a log rebuilt from the chains) sets it to the tip.
	_, err = c.adopt(s)
	require.NoError(err)
	reread, err := openSigningLog(c.log.path)
	require.NoError(err)
	want, err := tips(s)
	require.NoError(err)
	require.Equal(want[chainDogecoinVM], reread.Anchors[chainDogecoinVM])
	require.Equal(want[chainDogecoin], reread.Anchors[chainDogecoin])
	// Adopted: unproven, so a cutover first (round 5, U1; cutover.go).
	require.Empty(reread.History)
	require.Contains(reread.Cutover, chainDogecoinVM)
	require.Contains(reread.Cutover, chainDogecoin)
	_, _, _, err = c.check(req)
	require.ErrorContains(err, "signs only fences")
	c.log.Cutover, c.log.History = nil, historyFenced // as once its cutover is done
	_, _, _, err = c.check(req)
	require.NoError(err)
}

// TestLogKnowsTransactionsBySignedID (Codex r3 #5): signing a P2SH input
// changes the transaction's ID, so the log's unsigned ID is not what the
// chains list. The signer records the ID it sees, and its node still
// answers by it after its wallet stops listing the transaction: a second
// release of the same deposit is refused, though its inputs look spent.
func TestLogKnowsTransactionsBySignedID(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice := h.user(1)
	dep := h.deposit(10*doge, &alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()
	sent := h.vm.txs[len(h.vm.txs)-1].tx
	require.NotEqual(sent.TxHash(), unsignedHash(sent), "P2SH signatures change the ID")

	c := h.signers[0]
	s, err := c.b.load()
	require.NoError(err)
	nodes := func(string) chain { return h.vm }
	require.NoError(c.learnSigned(newChainEvidence(s, nodes)))
	key := actionRelease + ":" + wire.OutPoint{Hash: dep.TxHash()}.String()
	entry := c.log.Actions[key].Txs[0]
	require.Equal(unsignedHash(sent).String(), entry.Txid)
	require.Equal([]string{sent.TxHash().String()}, entry.Signed)
	reread, err := openSigningLog(c.log.path)
	require.NoError(err)
	require.Equal(entry.Signed, reread.Actions[key].Txs[0].Signed, "written")

	// The wallet no longer lists it; the node does, by the recorded ID.
	blind := &chainEvidence{listed: map[string]map[string]chainTx{}, nodes: nodes}
	require.True(blind.confirmed(chainDogecoinVM, entry))
	require.False(blind.confirmed(chainDogecoinVM, loggedTx{Txid: entry.Txid}), "not by the unsigned ID")
	again := wire.NewMsgTx(1)
	again.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{7}}, nil, nil))
	gone := func(wire.OutPoint) bool { return false }
	require.ErrorContains(c.log.permit(key, chainDogecoinVM, again, map[wire.OutPoint]bool{}, blind, gone), "in a block")
}

// TestBumpIsExactlyOneFee (Codex r3 #10): signers take a replacement that
// pays exactly one -doge-fee more than the payment it replaces, no more.
func TestBumpIsExactlyOneFee(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice, bob := h.user(1), h.user(2)
	h.deposit(50*doge, &alice, 9)
	for h.step() != "" {
		h.vm.mine()
	}
	h.pegOut(20*doge, bob)
	require.Contains(h.step(), "paid")
	s, err := h.b.load()
	require.NoError(err)
	r, oldFee, ok := s.unconfirmedFor(encodePayment(s.pegOuts[0].txid))
	require.True(ok)
	bump := func(fee int64) signRequest {
		tx, err := h.b.buildPayout(s.coinsOf(r.tx), s.pegOuts[0].value, bob, encodePayment(s.pegOuts[0].txid), fee)
		require.NoError(err)
		return signRequest{Chain: chainDogecoin, Fee: fee, Tx: encodeTx(tx),
			Action: action{Kind: actionPayout, PegOut: s.pegOuts[0].txid.String()}}
	}
	_, _, _, err = h.signers[0].check(bump(oldFee + 2*h.b.dogeFee))
	require.ErrorContains(err, "exactly one -doge-fee more", "within the bound, but two steps at once")
	_, _, _, err = h.signers[0].check(bump(oldFee + h.b.dogeFee))
	require.NoError(err)
}

// TestEveryLoggedKindLoadsAgain (Grok r3 N2): every action a signer
// records loads again when it restarts; a cancellation once didn't, and a
// signer that signed one could never start again.
func TestEveryLoggedKindLoadsAgain(t *testing.T) {
	require := require.New(t)
	l, err := createSigningLog(filepath.Join(t.TempDir(), "signing-log.json"))
	require.NoError(err)
	op := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 2}
	kinds := []action{
		{Kind: actionRelease, Deposit: op.String()},
		{Kind: actionRefund, Deposit: op.String()},
		{Kind: actionCancel, Deposit: op.String()},
		{Kind: actionPayout, PegOut: chainhash.Hash{3}.String()},
	}
	for _, a := range kinds {
		key, err := a.key()
		require.NoError(err)
		tx := wire.NewMsgTx(1)
		tx.AddTxIn(wire.NewTxIn(&op, nil, nil))
		require.NoError(l.record(key, chainDogecoin, tx, 1, 1))
	}
	reread, err := openSigningLog(l.path)
	require.NoError(err)
	require.Len(reread.Actions, len(kinds))

	// And through the signers: a forced refund's cancellation, then a
	// restart of every signer.
	h := newCosignHarness(t)
	h.setPolicy(func(b *bridge) { b.maxCirculating = 1 * doge })
	alice, back := h.user(1), h.user(9)
	dep := h.deposit(5*doge, &alice, 6)
	dop := wire.OutPoint{Hash: dep.TxHash()}
	h.approveRefunds(dop, back, " force")
	_, err = h.b.refund(dop, back, true)
	require.ErrorIs(err, errCancelSent)
	for _, c := range h.signers {
		_, err := openSigningLog(c.log.path)
		require.NoError(err)
	}
	raw, err := os.ReadFile(h.signers[0].log.path)
	require.NoError(err)
	var logged signingLog
	require.NoError(json.Unmarshal(raw, &logged))
	require.Contains(logged.Actions, actionCancel+":"+dop.String())
}

// TestMetalHealthMustBeRead (Codex r3 #8): a Metal node whose health can't
// be read, or answers without a verdict, fails the check.
func TestMetalHealthMustBeRead(t *testing.T) {
	require := require.New(t)
	serve := func(code int, body string) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			_, _ = w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv.URL + "/ext/bc/P"
	}
	require.NoError(metalHealthy(serve(200, `{"healthy":true}`)))
	require.ErrorContains(metalHealthy(serve(503, `{"healthy":false}`)), "unhealthy")
	require.ErrorContains(metalHealthy(serve(502, `bad gateway`)), "without a verdict")
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL
	closed.Close()
	require.ErrorContains(metalHealthy(url+"/ext/bc/P"), "can't be read")
}

// TestCreditToTheReserveIsHeld: a deposit naming the reserve itself as its
// destination would credit coins that look like reserve change; it is
// held for a refund instead, and nothing pays the peg address.
func TestCreditToTheReserveIsHeld(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	reserve, err := destinationOfScript(h.b.signers.pkScript())
	require.NoError(err)
	dep := h.deposit(10*doge, &reserve, 6)
	s, err := h.b.load()
	require.NoError(err)
	d, held := findDeposit(s.held, wire.OutPoint{Hash: dep.TxHash()})
	require.True(held)
	require.True(d.hasDest)
	_, err = h.b.buildPayout([]utxo{*s.pegAnchor}, doge/2, reserve, nil, doge/10)
	require.ErrorContains(err, "never pays the peg address")
}

// TestLineage: the anchor's line starts at the oldest coin at the script
// deep enough, moves only along confirmed spends to their last output at
// the script, ignores coins anyone else pays there however old they look
// to the signers' own transactions, and starts again at a later deep coin
// after a spend that paid nothing back.
func TestLineage(t *testing.T) {
	require := require.New(t)
	peg := []byte{0xa9, 0x14, 0xaa}
	other := []byte{0x51}
	pay := func(from *wire.OutPoint, outs ...[]byte) *wire.MsgTx {
		tx := wire.NewMsgTx(1)
		if from == nil {
			from = &wire.OutPoint{Hash: chainhash.Hash{byte(len(outs)), 0xee}}
		}
		tx.AddTxIn(wire.NewTxIn(from, nil, nil))
		for _, o := range outs {
			tx.AddTxOut(wire.NewTxOut(1000, o))
		}
		return tx
	}
	at := func(tx *wire.MsgTx, i uint32) wire.OutPoint { return wire.OutPoint{Hash: tx.TxHash(), Index: i} }

	seed := pay(nil, peg)                         // the root: 20 confirmations
	shallow := pay(&wire.OutPoint{Index: 7}, peg) // too shallow to be a root
	p1 := pay(&wire.OutPoint{Hash: seed.TxHash()}, other, peg, peg)
	stray := pay(&wire.OutPoint{Index: 9}, peg) // someone's payment, older than p1's change
	txs := []chainTx{{tx: seed, confirmations: 20}, {tx: shallow, confirmations: 2}, {tx: stray, confirmations: 15}, {tx: p1, confirmations: 10}}
	line := newLineage(txs, peg, 6)
	tip, root, moved, ok, err := line.tip()
	require.NoError(err)
	require.True(ok && moved)
	require.Equal(at(seed, 0), root)
	require.Equal(at(p1, 2), tip, "the last output to the peg: the change")

	// A spend still in the mempool doesn't move it.
	p2 := pay(&wire.OutPoint{Hash: p1.TxHash(), Index: 2}, other, peg)
	tip, _, _, _, err = newLineage(append(txs, chainTx{tx: p2}), peg, 6).tip()
	require.NoError(err)
	require.Equal(at(p1, 2), tip)

	// A confirmed spend paying nothing back: the line starts again at the
	// oldest deep coin in a later block (not the stray, which is older).
	dead := pay(&wire.OutPoint{Hash: p1.TxHash(), Index: 2}, other)
	later := pay(&wire.OutPoint{Index: 11}, peg)
	txs = append(txs, chainTx{tx: dead, confirmations: 9}, chainTx{tx: later, confirmations: 8})
	tip, _, _, ok, err = newLineage(txs, peg, 6).tip()
	require.NoError(err)
	require.True(ok)
	require.Equal(at(later, 0), tip)

	// No coin deep enough: no anchor yet.
	_, _, _, ok, err = newLineage([]chainTx{{tx: shallow, confirmations: 2}}, peg, 6).tip()
	require.NoError(err)
	require.False(ok)
}

// TestRegistryWritersDontLoseRegistrations: the bridge and the web server
// add to one registry file from separate processes (two registries here,
// each with its own mutex); with the file lock, every addition survives.
func TestRegistryWritersDontLoseRegistrations(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "deposits.json")
	writers := []*depositRegistry{{path: path}, {path: path}}
	const each = 60
	errs := make(chan error, 2*each)
	done := make(chan struct{})
	for w, r := range writers {
		go func() {
			defer func() { done <- struct{}{} }()
			for i := range each {
				var d destination
				d.hash[0], d.hash[1] = byte(w+1), byte(i)
				_, err := r.add(d)
				errs <- err
			}
		}()
	}
	<-done
	<-done
	close(errs)
	for err := range errs {
		require.NoError(err)
	}
	got, err := (&depositRegistry{path: path}).list()
	require.NoError(err)
	require.Len(got, 2*each, "no registration lost")
}
