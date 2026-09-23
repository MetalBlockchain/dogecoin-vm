package mempool

import (
	"bytes"
	"testing"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// newDogecoinPoolHarness returns a harness whose pool applies Dogecoin's
// default fee and dust policy.
func newDogecoinPoolHarness(t *testing.T) (*poolHarness, spendableOutput) {
	t.Helper()
	harness, outputs, err := newPoolHarness(&chaincfg.MainNetParams)
	if err != nil {
		t.Fatalf("unable to create test pool: %v", err)
	}
	harness.txPool.cfg.Policy.MinRelayTxFee = DefaultMinRelayTxFee
	harness.txPool.cfg.Policy.DustLimit = DefaultDustLimit
	harness.txPool.cfg.Policy.HardDustLimit = DefaultHardDustLimit
	return harness, outputs[0]
}

// createTx spends input to the given outputs, paying fee and sending the
// change back to the harness.
func createTx(t *testing.T, h *poolHarness, input spendableOutput,
	outputs []*wire.TxOut, fee int64) *btcutil.Tx {

	t.Helper()
	return createTxWithLockTime(t, h, input, outputs, fee, 0)
}

// createTxWithLockTime is createTx with a lock time. Every input is final, so
// the lock time has no effect beyond changing the signature.
func createTxWithLockTime(t *testing.T, h *poolHarness, input spendableOutput,
	outputs []*wire.TxOut, fee int64, lockTime uint32) *btcutil.Tx {

	t.Helper()
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.LockTime = lockTime
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: input.outPoint,
		Sequence:         wire.MaxTxInSequenceNum,
	})
	change := int64(input.amount) - fee
	for _, out := range outputs {
		tx.AddTxOut(out)
		change -= out.Value
	}
	tx.AddTxOut(wire.NewTxOut(change, h.payScript))

	sigScript, err := txscript.SignatureScript(tx, 0, h.payScript,
		txscript.SigHashAll, h.signKey, true)
	if err != nil {
		t.Fatal(err)
	}
	tx.TxIn[0].SignatureScript = sigScript
	return btcutil.NewTx(tx)
}

// sizeFee returns the relay fee Dogecoin requires for tx's size alone.
func sizeFee(tx *btcutil.Tx) int64 {
	return calcMinRequiredTxRelayFee(GetTxVirtualSize(tx), DefaultMinRelayTxFee)
}

// createTxAtBoundary returns a transaction whose fee is exactly its size fee
// plus surcharge plus delta. DER signatures vary in length by a byte, so the
// size of a signed transaction is not known in advance: this fixes the fee
// for a target size and varies the lock time until the size matches.
func createTxAtBoundary(t *testing.T, h *poolHarness, input spendableOutput,
	outputs []*wire.TxOut, surcharge, delta int64) *btcutil.Tx {

	t.Helper()
	size := GetTxVirtualSize(createTx(t, h, input, outputs, surcharge))
	fee := calcMinRequiredTxRelayFee(size, DefaultMinRelayTxFee) + surcharge + delta
	for lockTime := uint32(0); lockTime < 1000; lockTime++ {
		tx := createTxWithLockTime(t, h, input, outputs, fee, lockTime)
		if GetTxVirtualSize(tx) == size {
			return tx
		}
	}
	t.Fatalf("no lock time gives a %d byte transaction", size)
	return nil
}

func requireRejected(t *testing.T, h *poolHarness, tx *btcutil.Tx, code wire.RejectCode) {
	t.Helper()
	_, err := h.txPool.ProcessTransaction(tx, false, false, 0)
	if err == nil {
		t.Fatalf("transaction accepted, want reject code %v", code)
	}
	if got, ok := extractRejectCode(err); !ok || got != code {
		t.Fatalf("got error %v (code %v), want reject code %v", err, got, code)
	}
}

func requireAccepted(t *testing.T, h *poolHarness, tx *btcutil.Tx) {
	t.Helper()
	if _, err := h.txPool.ProcessTransaction(tx, false, false, 0); err != nil {
		t.Fatalf("transaction rejected: %v", err)
	}
}

func payTo(value int64, h *poolHarness) *wire.TxOut {
	return wire.NewTxOut(value, h.payScript)
}

// TestDogecoinRelayFee checks that every transaction must pay 0.001 DOGE/kB:
// Dogecoin has no free or priority relay.
func TestDogecoinRelayFee(t *testing.T) {
	h, input := newDogecoinPoolHarness(t)
	outputs := []*wire.TxOut{payTo(5e8, h)}

	requireRejected(t, h, createTx(t, h, input, outputs, 0), wire.RejectInsufficientFee)
	requireRejected(t, h, createTxAtBoundary(t, h, input, outputs, 0, -1), wire.RejectInsufficientFee)
	requireAccepted(t, h, createTxAtBoundary(t, h, input, outputs, 0, 0))
}

// TestDogecoinSoftDustFee checks that each output below 0.01 DOGE adds
// 0.01 DOGE to the required fee.
func TestDogecoinSoftDustFee(t *testing.T) {
	h, input := newDogecoinPoolHarness(t)
	outputs := []*wire.TxOut{payTo(500000, h), payTo(900000, h)} // 0.005 and 0.009 DOGE
	surcharge := 2 * int64(DefaultDustLimit)

	requireRejected(t, h, createTxAtBoundary(t, h, input, outputs, surcharge, -1), wire.RejectInsufficientFee)
	requireAccepted(t, h, createTxAtBoundary(t, h, input, outputs, surcharge, 0))
}

// TestDogecoinHardDust checks that an output below 0.001 DOGE is not
// standard, whatever fee is paid.
func TestDogecoinHardDust(t *testing.T) {
	h, input := newDogecoinPoolHarness(t)
	outputs := []*wire.TxOut{payTo(99999, h)}
	requireRejected(t, h, createTx(t, h, input, outputs, 1e8), wire.RejectDust)

	// Exactly the hard limit is allowed, paying the soft dust surcharge.
	outputs = []*wire.TxOut{payTo(100000, h)}
	requireAccepted(t, h, createTxAtBoundary(t, h, input, outputs, int64(DefaultDustLimit), 0))
}

// TestDogecoinOpReturn checks that an OP_RETURN output is never dust, and
// that only one is allowed per transaction.
func TestDogecoinOpReturn(t *testing.T) {
	h, input := newDogecoinPoolHarness(t)
	opReturn := func() *wire.TxOut {
		script, err := txscript.NullDataScript([]byte("data"))
		if err != nil {
			t.Fatal(err)
		}
		return wire.NewTxOut(0, script)
	}

	two := []*wire.TxOut{opReturn(), opReturn()}
	requireRejected(t, h, createTx(t, h, input, two, 1e8), wire.RejectNonstandard)

	one := []*wire.TxOut{opReturn()}
	requireAccepted(t, h, createTxAtBoundary(t, h, input, one, 0, 0))
}

// TestWitnessOutputsNotStandard checks that SegWit and Taproot outputs are
// kept out of the mempool. Neither is active on DogecoinVM, so such outputs
// would be spendable by anyone.
func TestWitnessOutputsNotStandard(t *testing.T) {
	program20 := bytes.Repeat([]byte{0x01}, 20)
	program32 := bytes.Repeat([]byte{0x02}, 32)

	scripts := map[string][]byte{
		"P2WPKH":          append([]byte{txscript.OP_0, txscript.OP_DATA_20}, program20...),
		"P2WSH":           append([]byte{txscript.OP_0, txscript.OP_DATA_32}, program32...),
		"P2TR":            append([]byte{txscript.OP_1, txscript.OP_DATA_32}, program32...),
		"unknown witness": append([]byte{txscript.OP_2, txscript.OP_DATA_20}, program20...),
	}
	for name, script := range scripts {
		t.Run(name, func(t *testing.T) {
			h, input := newDogecoinPoolHarness(t)
			outputs := []*wire.TxOut{wire.NewTxOut(5e8, script)}
			requireRejected(t, h, createTx(t, h, input, outputs, 1e8), wire.RejectNonstandard)
		})
	}
}
