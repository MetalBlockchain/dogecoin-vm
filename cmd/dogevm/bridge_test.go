package main

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// fakeChain is an in-memory ledger: it checks that inputs exist and are
// unspent, but not signatures.
type fakeChain struct {
	txs []*chainTx
}

func (c *fakeChain) output(op wire.OutPoint) (*wire.TxOut, bool) {
	for _, t := range c.txs {
		if t.tx.TxHash() == op.Hash && int(op.Index) < len(t.tx.TxOut) {
			return t.tx.TxOut[op.Index], true
		}
	}
	return nil, false
}

func (c *fakeChain) spent(op wire.OutPoint) bool {
	for _, t := range c.txs {
		for _, in := range t.tx.TxIn {
			if in.PreviousOutPoint == op {
				return true
			}
		}
	}
	return false
}

func (c *fakeChain) txsFor(addresses []btcutil.Address) ([]chainTx, error) {
	scripts := scriptSet(addresses)
	var out []chainTx
	for _, t := range c.txs {
		match := false
		for _, o := range t.tx.TxOut {
			match = match || scripts[string(o.PkScript)]
		}
		for _, in := range t.tx.TxIn {
			if prev, ok := c.output(in.PreviousOutPoint); ok && scripts[string(prev.PkScript)] {
				match = true
			}
		}
		if match {
			out = append(out, *t)
		}
	}
	return out, nil
}

func (c *fakeChain) unspent(addresses []btcutil.Address, minConf int64) ([]utxo, error) {
	scripts := scriptSet(addresses)
	var out []utxo
	for _, t := range c.txs {
		if t.confirmations < minConf {
			continue
		}
		hash := t.tx.TxHash()
		for i, o := range t.tx.TxOut {
			op := wire.OutPoint{Hash: hash, Index: uint32(i)}
			if scripts[string(o.PkScript)] && !c.spent(op) {
				out = append(out, utxo{outPoint: op, value: o.Value, pkScript: o.PkScript, confirmations: t.confirmations})
			}
		}
	}
	return out, nil
}

func (c *fakeChain) send(tx *wire.MsgTx) (chainhash.Hash, error) {
	var in, out int64
	for i, txIn := range tx.TxIn {
		prev, ok := c.output(txIn.PreviousOutPoint)
		if !ok || c.spent(txIn.PreviousOutPoint) {
			return chainhash.Hash{}, fmt.Errorf("input %v missing or spent", txIn.PreviousOutPoint)
		}
		in += prev.Value
		// Run the real script engine, so signing bugs fail the tests.
		vm, err := txscript.NewEngine(prev.PkScript, tx, i, txscript.StandardVerifyFlags,
			nil, nil, prev.Value, txscript.NewCannedPrevOutputFetcher(prev.PkScript, prev.Value))
		if err != nil {
			return chainhash.Hash{}, err
		}
		if err := vm.Execute(); err != nil {
			return chainhash.Hash{}, fmt.Errorf("input %d does not verify: %w", i, err)
		}
	}
	for _, o := range tx.TxOut {
		out += o.Value
	}
	if out > in {
		return chainhash.Hash{}, errors.New("outputs exceed inputs")
	}
	c.txs = append(c.txs, &chainTx{tx: tx})
	return tx.TxHash(), nil
}

// add puts tx on the chain with the given confirmations, without checks.
func (c *fakeChain) add(tx *wire.MsgTx, confirmations int64) {
	c.txs = append(c.txs, &chainTx{tx: tx, confirmations: confirmations})
}

func (c *fakeChain) mine() {
	for _, t := range c.txs {
		t.confirmations++
	}
}

type harness struct {
	t        *testing.T
	b        *bridge
	vm, doge *fakeChain
	nextCoin uint32
}

const doge = koinuPerDoge

func newHarness(t *testing.T) *harness {
	signers, err := newSignerSet(2, 3)
	require.NoError(t, err)
	h := &harness{t: t, vm: &fakeChain{}, doge: &fakeChain{}}
	h.b = &bridge{
		signers: signers, vm: h.vm, doge: h.doge,
		vmParams: &dogecoinTestNet, dogeParams: &dogecoinRegTest,
		depositConfirmations: 6,
		vmFee:                doge / 100,
		dogeFee:              doge,
		minDeposit:           doge,
		minPegOut:            2 * doge,
		logf:                 t.Logf,
	}
	h.b.registry = &depositRegistry{path: t.TempDir() + "/deposits.json"}
	var err2 error
	h.b.vmParams, err2 = dogevmParams("testnet")
	require.NoError(t, err2)

	// The consensus-created reserve.
	coinbase := wire.NewMsgTx(1)
	coinbase.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{}, wire.MaxPrevOutIndex), []byte{1}, nil))
	coinbase.AddTxOut(wire.NewTxOut(9_000_000_000*doge, signers.pkScript()))
	h.vm.add(coinbase, 1)
	return h
}

// coin returns a fresh funding outpoint from nowhere, for user payments the
// fake chain does not check.
func (h *harness) coin() *wire.OutPoint {
	h.nextCoin++
	return wire.NewOutPoint(&chainhash.Hash{0xff, byte(h.nextCoin)}, 0)
}

func (h *harness) user(b byte) destination {
	var d destination
	d.hash[0] = b
	return d
}

// deposit makes a Dogecoin deposit to the peg, tagged for dest if non-nil.
func (h *harness) deposit(value int64, dest *destination, confirmations int64) *wire.MsgTx {
	tx := wire.NewMsgTx(1)
	tx.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
	tx.AddTxOut(wire.NewTxOut(value, h.b.signers.pkScript()))
	if dest != nil {
		tx.AddTxOut(nullData(encodeDestination(tagDeposit, *dest)))
	}
	h.doge.add(tx, confirmations)
	return tx
}

// pegOut makes a DogecoinVM payment to the reserve asking for dest.
func (h *harness) pegOut(value int64, dest destination) *wire.MsgTx {
	tx := wire.NewMsgTx(1)
	tx.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
	tx.AddTxOut(wire.NewTxOut(value, h.b.signers.pkScript()))
	tx.AddTxOut(nullData(encodeDestination(tagPegOut, dest)))
	h.vm.add(tx, 1)
	return tx
}

func (h *harness) step() string {
	h.t.Helper()
	did, err := h.b.step()
	require.NoError(h.t, err)
	return did
}

func (h *harness) audit() audit {
	s, err := h.b.load()
	require.NoError(h.t, err)
	return h.b.audit(s)
}

// paidTo returns what the last transaction on c pays to dest.
func paidTo(c *fakeChain, dest destination) int64 {
	var total int64
	for _, o := range c.txs[len(c.txs)-1].tx.TxOut {
		if bytes.Equal(o.PkScript, dest.pkScript()) {
			total += o.Value
		}
	}
	return total
}

func TestPegInCreditsOnceAfterConfirmations(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice := h.user(1)

	h.deposit(100*doge, &alice, 5)
	require.Empty(h.step(), "credited before 6 confirmations")

	h.doge.mine()
	require.NotEmpty(h.step())
	require.Equal(int64(100*doge-doge/100), paidTo(h.vm, alice))

	// While the release is in the mempool, and after it is accepted,
	// the deposit is not credited again.
	require.Empty(h.step())
	h.vm.mine()
	require.Empty(h.step())

	a := h.audit()
	require.True(a.solvent())
	require.Equal(int64(100*doge), a.Circulating)
	require.Equal(int64(100*doge), a.Locked)
	require.Zero(a.PendingPegIns)
	require.Zero(a.Surplus)
}

func TestSpoofedReleaseTagIsIgnored(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice := h.user(1)
	dep := h.deposit(100*doge, &alice, 6)

	// Anyone can pay the reserve with a release tag; it must not count as
	// crediting the deposit.
	spoof := wire.NewMsgTx(1)
	spoof.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
	spoof.AddTxOut(wire.NewTxOut(doge, h.b.signers.pkScript()))
	spoof.AddTxOut(nullData(encodeRelease(wire.OutPoint{Hash: dep.TxHash(), Index: 0})))
	h.vm.add(spoof, 1)

	require.NotEmpty(h.step())
	require.Equal(int64(100*doge-doge/100), paidTo(h.vm, alice))
}

func TestRoundTrip(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice, aliceOnDoge := h.user(1), h.user(2)

	h.deposit(100*doge, &alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()

	req := h.pegOut(40*doge, aliceOnDoge)
	require.NotEmpty(h.step())
	require.Equal(int64(39*doge), paidTo(h.doge, aliceOnDoge), "40 DOGE less the 1 DOGE fee")
	payment := h.doge.txs[len(h.doge.txs)-1].tx
	paidFor, ok := parsePayment(payment)
	require.True(ok)
	require.Equal(req.TxHash(), paidFor)

	// Paid once, even before the payment confirms.
	require.Empty(h.step())
	h.doge.mine()
	require.Empty(h.step())

	a := h.audit()
	require.True(a.solvent(), "%+v", a)
	require.Equal(int64(60*doge), a.Circulating)
	require.Equal(int64(60*doge), a.Locked)
	require.Zero(a.Surplus)
}

func TestUntaggedDepositsAreNotCredited(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice := h.user(1)

	h.deposit(100*doge, nil, 6)  // no destination
	h.deposit(doge/2, &alice, 6) // below the minimum
	require.Empty(h.step())

	a := h.audit()
	require.True(a.solvent())
	require.Zero(a.Circulating)
	require.Equal(int64(100*doge+doge/2), a.UnclaimedOnDoge)
	require.Equal(a.UnclaimedOnDoge, a.Surplus)
}

func TestBridgeHaltsWhenInsolvent(t *testing.T) {
	h := newHarness(t)
	alice := h.user(1)

	// Reserve released with nothing locked on Dogecoin, as if a signer key
	// were compromised.
	reserve, err := h.vm.unspent([]btcutil.Address{mustAddr(h.b.vmReserveAddress())}, 1)
	require.NoError(t, err)
	theft := wire.NewMsgTx(1)
	theft.AddTxIn(wire.NewTxIn(&reserve[0].outPoint, nil, nil))
	theft.AddTxOut(wire.NewTxOut(50*doge, alice.pkScript()))
	theft.AddTxOut(wire.NewTxOut(reserve[0].value-50*doge, h.b.signers.pkScript()))
	h.vm.add(theft, 1)
	h.deposit(10*doge, &alice, 6)

	_, err = h.b.step()
	require.ErrorIs(t, err, errInsolvent)
}

func mustAddr(a btcutil.Address, err error) btcutil.Address {
	if err != nil {
		panic(err)
	}
	return a
}

func TestTagsRoundTrip(t *testing.T) {
	require := require.New(t)
	d := destination{kind: destP2SH}
	d.hash[19] = 9

	tx := wire.NewMsgTx(1)
	tx.AddTxOut(nullData(encodeDestination(tagPegOut, d)))
	got, ok := parseDestinationTag(tx, tagPegOut)
	require.True(ok)
	require.Equal(d, got)
	_, ok = parseDestinationTag(tx, tagDeposit)
	require.False(ok, "wrong tag")

	// Two OP_RETURNs are ambiguous and ignored.
	tx.AddTxOut(nullData(encodeDestination(tagPegOut, d)))
	_, ok = parseDestinationTag(tx, tagPegOut)
	require.False(ok)
}

func TestParseDoge(t *testing.T) {
	for in, want := range map[string]int64{
		"1": doge, "0.00000001": 1, "123.45": 12345 * doge / 100, "10000000000": 10_000_000_000 * doge,
	} {
		got, err := parseDoge(in)
		require.NoError(t, err, in)
		require.Equal(t, want, got, in)
		require.Equal(t, in, trimZeros(formatDoge(got)), in)
	}
	for _, bad := range []string{"", "1.2.3", "-1", "abc", "10000000001"} {
		_, err := parseDoge(bad)
		require.Error(t, err, bad)
	}
}

func trimZeros(s string) string {
	s = string(bytes.TrimRight([]byte(s), "0"))
	return string(bytes.TrimSuffix([]byte(s), []byte(".")))
}

// personalDeposit pays value to dest's personal deposit address, with no
// OP_RETURN, as any Dogecoin wallet would.
func (h *harness) personalDeposit(value int64, dest destination, confirmations int64) *wire.MsgTx {
	tx := wire.NewMsgTx(1)
	tx.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
	tx.AddTxOut(wire.NewTxOut(value, p2shScript(h.b.signers.depositRedeemScript(dest))))
	h.doge.add(tx, confirmations)
	return tx
}

func TestPersonalDepositAddress(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice, bob := h.user(1), h.user(2)

	// Each destination gets its own address, distinct from the peg address.
	aliceAddr, err := h.b.signers.depositAddress(alice, h.b.dogeParams)
	require.NoError(err)
	bobAddr, err := h.b.signers.depositAddress(bob, h.b.dogeParams)
	require.NoError(err)
	pegAddr, _ := h.b.dogePegAddress()
	require.NotEqual(aliceAddr.EncodeAddress(), bobAddr.EncodeAddress())
	require.NotEqual(pegAddr.EncodeAddress(), aliceAddr.EncodeAddress())

	// Unregistered, the bridge does not watch it.
	h.personalDeposit(100*doge, alice, 6)
	require.Empty(h.step())

	added, err := h.b.registry.add(alice)
	require.NoError(err)
	require.True(added)
	added, err = h.b.registry.add(alice)
	require.NoError(err)
	require.False(added, "registering twice is a no-op")

	require.NotEmpty(h.step())
	require.Equal(int64(100*doge-doge/100), paidTo(h.vm, alice))
	h.vm.mine()
	require.Empty(h.step())
}

// TestPegOutSpendsPersonalDeposit checks that the signers can spend a
// personal deposit output: fakeChain runs the script engine on it.
func TestPegOutSpendsPersonalDeposit(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice, aliceOnDoge := h.user(1), h.user(2)
	_, err := h.b.registry.add(alice)
	require.NoError(err)

	h.personalDeposit(100*doge, alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()

	h.pegOut(60*doge, aliceOnDoge)
	did, err := h.b.step()
	require.NoError(err)
	require.NotEmpty(did)
	require.Equal(int64(59*doge), paidTo(h.doge, aliceOnDoge))

	h.doge.mine()
	a := h.audit()
	require.True(a.solvent(), "%+v", a)
	require.Equal(int64(40*doge), a.Circulating)
	require.Equal(int64(40*doge), a.Locked)
}

func TestDepositCaps(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	h.b.maxDeposit = 100 * doge
	h.b.maxCirculating = 150 * doge
	alice := h.user(1)

	// Over the per-deposit cap: never credited, held on Dogecoin.
	h.deposit(101*doge, &alice, 6)
	require.Empty(h.step())
	a := h.audit()
	require.Equal(int64(101*doge), a.UnclaimedOnDoge)

	// Within both caps: credited.
	h.deposit(100*doge, &alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()

	// Would take circulating to 180 DOGE, past the 150 cap: waits.
	h.deposit(80*doge, &alice, 6)
	require.Empty(h.step())
	a = h.audit()
	require.Equal(int64(100*doge), a.Circulating)
	require.Equal(int64(80*doge), a.PendingPegIns)
	require.True(a.solvent())

	// Raising the cap releases it.
	h.b.maxCirculating = 200 * doge
	require.NotEmpty(h.step())
}

func TestRefundHeldDeposit(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	h.b.maxDeposit = 100 * doge
	alice, aliceOnDoge := h.user(1), h.user(2)

	// Two deposits so the peg has a confirmed output to pay from, one of
	// them over the cap.
	h.deposit(50*doge, &alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()
	over := h.deposit(150*doge, &alice, 6)
	op := wire.OutPoint{Hash: over.TxHash(), Index: 0}
	require.Empty(h.step())
	require.Equal(int64(150*doge), h.audit().UnclaimedOnDoge)

	_, err := h.b.refund(op, aliceOnDoge, false)
	require.NoError(err)
	require.Equal(int64(149*doge), paidTo(h.doge, aliceOnDoge), "150 DOGE less the 1 DOGE fee")
	refundTx := h.doge.txs[len(h.doge.txs)-1].tx
	refunded, ok := parseRefund(refundTx)
	require.True(ok)
	require.Equal(op, refunded)

	// Settled: no longer held, never credited, and cannot be refunded again.
	h.doge.mine()
	a := h.audit()
	require.Zero(a.UnclaimedOnDoge)
	require.True(a.solvent(), "%+v", a)
	require.Equal(int64(50*doge), a.Circulating)
	require.Equal(int64(50*doge), a.Locked)
	_, err = h.b.refund(op, aliceOnDoge, false)
	require.ErrorContains(err, "already refunded")

	h.b.maxDeposit = 0 // even with the cap lifted, it stays refunded
	require.Empty(h.step())
}

func TestRefundCreditableDepositNeedsForce(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice, aliceOnDoge := h.user(1), h.user(2)

	// Waiting for confirmations: the bridge would credit it later.
	dep := h.deposit(40*doge, &alice, 1)
	h.doge.add(func() *wire.MsgTx { // a confirmed peg output to pay from
		tx := wire.NewMsgTx(1)
		tx.AddTxIn(wire.NewTxIn(h.coin(), nil, nil))
		tx.AddTxOut(wire.NewTxOut(100*doge, h.b.signers.pkScript()))
		return tx
	}(), 6)
	op := wire.OutPoint{Hash: dep.TxHash(), Index: 0}

	_, err := h.b.refund(op, aliceOnDoge, false)
	require.ErrorIs(err, errCreditable)

	_, err = h.b.refund(op, aliceOnDoge, true)
	require.NoError(err)
	for i := 0; i < 6; i++ {
		h.doge.mine()
	}
	require.Empty(h.step(), "a refunded deposit is never credited")

	_, err = h.b.refund(wire.OutPoint{Index: 9}, aliceOnDoge, false)
	require.ErrorIs(err, errNotRefundable)
}
