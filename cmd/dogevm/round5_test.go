package main

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// Round 5 (U1–U6, U11): one cutover, recorded coin by coin, the same shape
// as BTCVM's and LTCVM's (FENCE-NAMES.md).

// rebuildLogs is the cutover after the signers' logs were lost (or never
// kept, as on the live set): each signer starts a new log and adopts what
// the chains show it signed, which starts a cutover on both chains.
func (h *cosignHarness) rebuildLogs() {
	h.t.Helper()
	for _, c := range h.signers {
		c.log = &signingLog{path: filepath.Join(h.t.TempDir(), "signing-log.json"), Actions: map[string]*loggedAction{}}
		s, err := c.b.load()
		require.NoError(h.t, err)
		_, err = c.adopt(s)
		require.NoError(h.t, err)
		require.Empty(h.t, c.log.History, "adopted: unproven")
		require.Contains(h.t, c.log.Cutover, chainDogecoin)
		require.Contains(h.t, c.log.Cutover, chainDogecoinVM)
		require.Equal(h.t, 1, c.log.CutoverEpoch)
	}
}

// oldLogs is logs written by the software before the cutover rule: no
// History, anchors at today's tips (they walk), nothing else to tell.
func (h *cosignHarness) oldLogs() {
	h.t.Helper()
	for _, c := range h.signers {
		c.log.History = ""
		require.NoError(h.t, c.log.write())
	}
}

// fenceUntilDone proposes fences on both chains and mines, until every
// signer's cutover is done (or rounds run out).
func (h *cosignHarness) fenceUntilDone(rounds int) {
	h.t.Helper()
	for range rounds {
		for _, chain := range cutoverChains {
			_, _ = h.b.proposeFence(chain)
		}
		h.vm.mine()
		h.doge.mine()
		done := true
		for _, c := range h.signers {
			s, err := c.b.load()
			require.NoError(h.t, err)
			_, err = c.advanceCutover(s)
			require.NoError(h.t, err)
			done = done && !c.log.unproven()
		}
		if done {
			return
		}
	}
	h.t.Fatalf("the cutover didn't finish in %d rounds", rounds)
}

// gated reports the error a signer gives a release of dep now.
func (h *cosignHarness) releaseErr(c *cosigner, dep *wire.MsgTx, dest destination) error {
	h.t.Helper()
	s, err := c.b.load()
	require.NoError(h.t, err)
	d, ok := findDeposit(s.deposits, wire.OutPoint{Hash: dep.TxHash()})
	require.True(h.t, ok)
	inputs, total, err := s.releaseInputs(d.value)
	require.NoError(h.t, err)
	tx := h.b.buildRelease(inputs, total, d)
	_, _, _, err = c.check(signRequest{Chain: chainDogecoinVM, Tx: encodeTx(tx), Register: []string{encodeDest(dest)},
		Action: action{Kind: actionRelease, Deposit: d.outPoint.String()}})
	return err
}

// keptBack is a payout and a release signed by signers 0 and 1 the way the
// software before logs did (any coins, off today's lines), assembled and
// kept back.
func (h *cosignHarness) keptBack(capital, req, dep2 *wire.MsgTx, bob, alice destination) (payout, release *wire.MsgTx) {
	h.t.Helper()
	s, err := h.b.load()
	require.NoError(h.t, err)
	p, ok := findPegOut(s.pegOuts, req.TxHash())
	require.True(h.t, ok)
	capitalCoin := wire.OutPoint{Hash: capital.TxHash()}
	require.NotEqual(h.t, capitalCoin, s.pegAnchor.outPoint)
	payout = wire.NewMsgTx(1)
	payout.AddTxIn(wire.NewTxIn(&capitalCoin, nil, nil))
	payout.AddTxOut(wire.NewTxOut(p.value-h.b.dogeFee, bob.pkScript()))
	payout.AddTxOut(wire.NewTxOut(capital.TxOut[0].Value-p.value, h.b.signers.pkScript()))
	payout.AddTxOut(nullData(encodePayment(p.txid)))
	payout = h.oldSignature(payout, [][]byte{h.b.signers.redeemScript})

	reserveCoin := wire.OutPoint{Hash: req.TxHash()}
	require.NotEqual(h.t, reserveCoin, s.reserveAnchor.outPoint)
	release = wire.NewMsgTx(wire.TxVersion)
	release.AddTxIn(wire.NewTxIn(&reserveCoin, nil, nil))
	release.AddTxOut(wire.NewTxOut(4*doge-h.b.vmFee, alice.pkScript()))
	release.AddTxOut(wire.NewTxOut(doge, h.b.signers.pkScript()))
	release.AddTxOut(nullData(encodeRelease(wire.OutPoint{Hash: dep2.TxHash()})))
	release = h.oldSignature(release, [][]byte{h.b.signers.redeemScript})
	return payout, release
}

// setup is a bridge in use: capital, a credited deposit, a peg-out paid
// into the reserve.
func (h *cosignHarness) inUse() (alice, bob destination, capital, req *wire.MsgTx) {
	alice, bob = h.user(1), h.user(2)
	capital = h.contribute(10*doge, 50) // operating capital for the fences' fees
	h.deposit(10*doge, &alice, 9)
	for h.step() != "" {
		h.vm.mine()
	}
	req = h.pegOut(5*doge, bob)
	h.vm.mine()
	return alice, bob, capital, req
}

// TestFenceKillsKeptBackSignatures (round 4 T5, round 5 U1–U3): signatures
// the logless signers gave and the coordinator kept back can spend any
// coins. Until every coin recorded at the cutover is spent deep, no signer
// signs anything but fences; then the kept-back transactions can never
// confirm, the logs are fenced, and the bridge carries on.
func TestFenceKillsKeptBackSignatures(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice, bob, capital, req := h.inUse()
	dep2 := h.deposit(4*doge, &alice, 9)
	payout, release := h.keptBack(capital, req, dep2, bob, alice)

	h.rebuildLogs()
	_, err := h.b.step()
	require.ErrorContains(err, "signs only fences", "nothing but fences until the cutover is done")

	_, err = h.b.proposeFence(chainDogecoin)
	require.NoError(err)
	_, err = h.b.proposeFence(chainDogecoinVM)
	require.ErrorContains(err, "no other fee is paid from the surplus", "round 8, X2: the DogecoinVM fence waits for Dogecoin's to be deep")
	h.vm.mine()
	h.doge.mine()
	require.ErrorContains(h.releaseErr(h.signers[2], dep2, alice), "in a cutover", "Dogecoin's fence must be deep first")
	h.fenceUntilDone(20)

	_, err = h.doge.send(payout)
	require.ErrorContains(err, "missing or spent", "the kept-back payout's coin is spent")
	_, err = h.vm.send(release)
	require.ErrorContains(err, "missing or spent", "the kept-back release's coin is spent")
	for _, c := range h.signers {
		require.Equal(historyFenced, c.log.History)
		require.Empty(c.log.Cutover)
	}

	did := ""
	for range 6 {
		got, err := h.b.step()
		require.NoError(err)
		did += got
		h.vm.mine()
		h.doge.mine()
	}
	require.Contains(did, "paid")
	a := h.audit()
	require.True(a.solvent(), "%+v", a)

	// A log rebuilt again later adopts the fences it signed, by their tags.
	c := h.signers[0]
	c.log = &signingLog{path: filepath.Join(t.TempDir(), "signing-log.json"), Actions: map[string]*loggedAction{}}
	cs, err := c.b.load()
	require.NoError(err)
	adopted, err := c.adopt(cs)
	require.NoError(err)
	require.NotEmpty(adopted)
	_, err = openSigningLog(c.log.path)
	require.NoError(err)
}

// TestOldLogThatWalksStillCutsOver (Grok r5 #1, U1): a log written by the
// software before the cutover rule, whose anchor already walks to today's
// tip, is still unproven: it may hold signatures from any coin. It starts a
// cutover by itself on its first request and signs nothing else until it is
// done. With the old rule (a fence only for anchors set or changed), these
// signers were never gated and the kept-back payout and a new release of
// the same peg-out could both confirm.
func TestOldLogThatWalksStillCutsOver(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice, bob, capital, req := h.inUse()
	dep2 := h.deposit(4*doge, &alice, 9)
	payout, _ := h.keptBack(capital, req, dep2, bob, alice)
	h.oldLogs()
	for _, c := range h.signers {
		s, err := c.b.load()
		require.NoError(err)
		_, err = c.fillAnchors(s)
		require.NoError(err, "the anchors walk to the tips")
	}
	require.ErrorContains(h.releaseErr(h.signers[2], dep2, alice), "in a cutover")
	require.Equal(1, h.signers[2].log.CutoverEpoch, "started by the request itself")
	_, err := h.b.step()
	require.ErrorContains(err, "signs only fences")

	h.fenceUntilDone(10)
	_, err = h.doge.send(payout)
	require.ErrorContains(err, "missing or spent")
	require.NoError(h.releaseErr(h.signers[2], dep2, alice))
}

// TestMultiBatchNeedsEveryCoinDeep (Codex r5 #1, U3): a cutover with more
// coins than one fence holds takes several. Completion needs every
// recorded coin spent deep, the last batch's too: a first deep fence plus
// a shallow spend of the rest isn't enough, as a reorganisation can undo
// the shallow one and bring back a kept-back signature's coin. A signer
// that didn't sign the last batch is held the same way.
func TestMultiBatchNeedsEveryCoinDeep(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.contribute(20*doge, 50)
	n := maxFenceInputs(chainDogecoin) + 5
	for range n {
		h.contribute(doge, 30)
	}
	alice := h.user(1)
	dep := h.deposit(4*doge, &alice, 30)
	h.rebuildLogs()
	recorded := recordedSet(h.signers[2].log.Cutover[chainDogecoin])
	require.Greater(len(recorded), maxFenceInputs(chainDogecoin))

	// VM done; Dogecoin batch 1, then deep.
	_, err := h.b.proposeFence(chainDogecoinVM)
	require.NoError(err)
	f1, err := h.b.proposeFence(chainDogecoin)
	require.NoError(err)
	h.vm.mine()
	h.doge.mine()
	_, err = h.b.proposeFence(chainDogecoin)
	require.ErrorContains(err, "not yet deep enough", "batch 2 waits for batch 1's depth")
	for range 10 {
		h.doge.mine()
	}
	// Batch 2, signed by signers 0 and 1 only, one block deep.
	var last *chainTx
	_, err = h.b.proposeFence(chainDogecoin)
	require.NoError(err)
	for _, t := range h.doge.txs {
		if _, ok := parseFence(t.tx); ok && t.tx.TxHash() != f1 && t.confirmations == 0 {
			last = t
		}
	}
	require.NotNil(last)
	h.doge.mine()
	c := h.signers[2]
	require.ErrorContains(h.releaseErr(c, dep, alice), "in a cutover", "batch 2 is shallow")

	// A reorganisation undoes batch 2: its coins are unspent again, and
	// nothing has cleared them.
	last.confirmations = 0
	require.ErrorContains(h.releaseErr(c, dep, alice), "in a cutover")
	h.doge.replace(last.tx) // dropped from the mempool too
	s, err := c.b.load()
	require.NoError(err)
	require.NotEmpty(c.cutoverPending(s)[chainDogecoin], "the batch's coins need a fence again")
	h.fenceUntilDone(20)
	require.NoError(h.releaseErr(c, dep, alice))
}

// TestDisconnectedCoinIsRecorded (Codex r5 #2, U2/U3): a coin whose
// transaction a reorganisation took out of its block before the cutover is
// unconfirmed in the wallet, not unspent in a block: a fence can't spend
// it. It is still recorded (a kept-back signature may spend it once its
// transaction is mined again), so the cutover can't finish until it is
// mined and fenced (or its transaction is conflicted deep). Round 6, V1:
// there is no waiver; time, absence from a mempool, and an old log's
// "cutoverWaived" record clear nothing.
func TestDisconnectedCoinIsRecorded(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.contribute(10*doge, 50)
	gone := h.contribute(2*doge, 0) // was in a block; disconnected
	alice := h.user(1)
	dep := h.deposit(4*doge, &alice, 30)
	h.rebuildLogs()
	goneCoin := wire.OutPoint{Hash: gone.TxHash()}
	c := h.signers[2]
	require.Contains(c.log.Cutover[chainDogecoin], goneCoin.String())

	for range 15 {
		_, _ = h.b.proposeFence(chainDogecoin)
		_, _ = h.b.proposeFence(chainDogecoinVM)
		h.vm.mine()
		h.doge.mineExcept(gone)
	}
	s, err := c.b.load()
	require.NoError(err)
	require.Equal([]string{goneCoin.String()}, c.cutoverLeft(s)[chainDogecoin], "only the disconnected coin is left")
	require.ErrorContains(h.releaseErr(c, dep, alice), "in a cutover")

	// A day on, still gated: nothing but a deep spend or a deep conflict
	// clears a coin. A round-5 log's waiver record is not read either.
	c.log.CutoverStarted = time.Now().Add(-25 * time.Hour).Unix()
	require.NoError(c.log.write())
	raw, err := json.Marshal(c.log)
	require.NoError(err)
	var doc map[string]any
	require.NoError(json.Unmarshal(raw, &doc))
	doc["cutoverWaived"] = map[string][]string{chainDogecoin: {goneCoin.String()}}
	raw, err = json.Marshal(doc)
	require.NoError(err)
	var reread signingLog
	require.NoError(json.Unmarshal(raw, &reread))
	c.log.Cutover = reread.Cutover
	require.Equal([]string{goneCoin.String()}, c.cutoverLeft(s)[chainDogecoin], "a waiver record clears nothing")
	require.ErrorContains(h.releaseErr(c, dep, alice), "in a cutover")

	// Once its transaction is mined again, a fence spends it and the
	// cutover ends the normal way.
	h.doge.mine()
	h.fenceUntilDone(20)
	require.NoError(h.releaseErr(c, dep, alice))
}

// mineExcept mines a block holding everything but skip.
func (c *fakeChain) mineExcept(skip *wire.MsgTx) {
	for _, t := range c.txs {
		if t.tx.TxHash() != skip.TxHash() {
			t.confirmations++
		}
	}
}

// TestNoFenceOutsideACutover (Grok r5 #2, U4): once a cutover is done, a
// signer signs no more fences, however often the coordinator asks: each
// would spend surplus that cancellations need.
func TestNoFenceOutsideACutover(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.inUse()
	h.rebuildLogs()
	h.fenceUntilDone(10)
	before := h.audit().Surplus
	for range 5 {
		_, err := h.b.proposeFence(chainDogecoin)
		require.ErrorContains(err, "no signer is in a cutover")
		h.doge.mine()
	}
	c := h.signers[2]
	s, err := c.b.load()
	require.NoError(err)
	_, _, _, err = c.checkFence(s, chainDogecoin)
	require.ErrorContains(err, "in no cutover")
	require.Equal(before, h.audit().Surplus, "no surplus spent")
}

// TestSpoofedFenceTagIsIgnored (Codex r5 #7, U4): anyone can pay the set's
// address with a fence's tag. Pending, it doesn't hold up the real fence;
// deep, it clears nothing.
func TestSpoofedFenceTagIsIgnored(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.inUse()
	h.rebuildLogs()
	s, err := h.b.load()
	require.NoError(err)
	fake := wire.NewMsgTx(1)
	fake.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
	fake.AddTxOut(wire.NewTxOut(doge, h.b.signers.pkScript()))
	fake.AddTxOut(nullData(encodeFence(s.pegAnchor.outPoint)))
	h.doge.add(fake, 0)
	_, err = h.b.proposeFence(chainDogecoin)
	require.NoError(err, "a stranger's pending 'fence' doesn't block the real one")

	// Deep, it spends nothing recorded, so it clears nothing.
	h.doge.add(fake, 0)
	for range 10 {
		h.doge.mine()
	}
	c := h.signers[2]
	s, err = c.b.load()
	require.NoError(err)
	pending, _ := fenceState(s, chainDogecoin, recordedSet(c.log.Cutover[chainDogecoin]))
	require.Nil(pending)
}

// TestFenceSpendsEveryCoinTheSignerSees (U4): a fence that leaves out a
// recorded coin could leave a kept-back signature alive, so the signer
// refuses it; and a signer in no cutover signs no fence.
func TestFenceSpendsEveryCoinTheSignerSees(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.inUse()
	c := h.signers[2]
	s, err := h.b.load()
	require.NoError(err)
	coins, err := cutoverCoins(s, &signingLog{Actions: map[string]*loggedAction{}})
	require.NoError(err)
	inputs, err := s.fenceInputs(chainDogecoin, recordedSet(coins[chainDogecoin]))
	require.NoError(err)
	require.Greater(len(inputs), 2)
	request := func(in []utxo) signRequest {
		size, err := h.b.fenceSizeOf(s, chainDogecoin, in)
		require.NoError(err)
		tx, err := h.b.buildFence(chainDogecoin, in, fenceFee(chainDogecoin, h.b.dogeFee, size))
		require.NoError(err)
		return signRequest{Chain: chainDogecoin, Tx: encodeTx(tx), Action: action{Kind: actionFence, Deposit: in[0].outPoint.String()}}
	}
	_, _, _, err = c.check(request(inputs))
	require.ErrorContains(err, "in no cutover", "a new key's log signs no fence")

	h.rebuildLogs()
	_, _, _, err = c.check(request(inputs[:len(inputs)-1]))
	require.ErrorContains(err, "not the transaction this signer would build")
	_, _, _, err = c.check(request(inputs))
	require.NoError(err)
}

// TestFencesAreSignedWhilePaused (U6): an upgrade leaves every signer
// paused; fences pay no one, so they are signed anyway, and nothing else.
func TestFencesAreSignedWhilePaused(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice, _, _, _ := h.inUse()
	dep := h.deposit(4*doge, &alice, 9)
	h.rebuildLogs()
	for _, c := range h.signers {
		dir := t.TempDir()
		c.b.signers.path = filepath.Join(dir, "signers.json")
		require.NoError(writePause(c.b.signers.path, pauseState{Reason: "upgrade", Since: time.Now()}))
	}
	h.fenceUntilDone(10)
	require.ErrorContains(h.releaseErr(h.signers[2], dep, alice), "paused")
}

// TestKeptBackPayoutSentBeforeTheFence (round 4 T5): if the coordinator
// sends a kept-back payout after the logs were rebuilt and before the
// fence, the signers that signed it see a signature their new log lacks and
// quarantine themselves: no fence is signed, and nothing else.
func TestKeptBackPayoutSentBeforeTheFence(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	_, bob, _, _ := h.inUse()
	req := h.pegOut(5*doge, bob)
	payout, redeems, sigs := h.keptPayout(req)
	h.rebuildLogs()
	require.NoError(h.b.signers.assemble(payout, redeems, sigs))
	_, err := h.doge.send(payout)
	require.NoError(err)
	h.doge.mine()

	_, err = h.b.proposeFence(chainDogecoin)
	require.Error(err)
	require.NotEmpty(h.signers[0].quarantined())
	require.NotEmpty(h.signers[1].quarantined())
}

// TestBrokenAnchorRecoversByCutover (round 5, replacing -refence): a log
// whose anchor no longer leads to the tip refuses anchors -set; the
// recovery is signer-log cutover, a new epoch, whose fences start from the
// tip whatever the anchor; when it is done, the anchor is the tip.
func TestBrokenAnchorRecoversByCutover(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	h.inUse()
	c := h.signers[2]
	broken := wire.OutPoint{Hash: chainhash.Hash{0xde, 0xad}}
	c.log.Anchors[chainDogecoin] = broken.String()
	s, err := c.b.load()
	require.NoError(err)
	_, err = c.fillAnchors(s)
	require.ErrorContains(err, "signer-log cutover")

	for _, sc := range h.signers {
		ss, err := sc.b.load()
		require.NoError(err)
		require.NoError(sc.startCutover(ss))
	}
	require.Equal(1, c.log.CutoverEpoch)
	h.fenceUntilDone(10)
	s, err = c.b.load()
	require.NoError(err)
	want, err := tips(s)
	require.NoError(err)
	require.Equal(want[chainDogecoin], c.log.Anchors[chainDogecoin])
	require.Equal(historyFenced, c.log.History)
}

// TestFenceGateCoversBothChains (round 4): while a cutover is unfinished on
// either chain, a signer signs nothing but fences on both. A release on a
// cleared DogecoinVM could otherwise confirm beside a refund or payout of
// the same deposit, kept back on a still uncleared Dogecoin.
func TestFenceGateCoversBothChains(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice, _, _, _ := h.inUse()
	dep := h.deposit(4*doge, &alice, 9)
	h.rebuildLogs()
	_, err := h.b.proposeFence(chainDogecoinVM) // only DogecoinVM
	require.NoError(err)
	h.vm.mine()

	c := h.signers[2]
	s, err := c.b.load()
	require.NoError(err)
	_, err = c.advanceCutover(s)
	require.NoError(err)
	require.NotContains(c.log.Cutover, chainDogecoinVM, "DogecoinVM's part is done")
	require.ErrorContains(h.releaseErr(c, dep, alice), "in a cutover", "a DogecoinVM release waits for Dogecoin")
}
