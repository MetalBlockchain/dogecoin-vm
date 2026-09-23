// Copyright (c) 2013-2016 The btcsuite developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package mempool

import (
	"bytes"
	"testing"
	"time"

	"github.com/MetalBlockchain/btcvm/btcd/btcec/v2"
	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/btcvm/btcd/txscript"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// TestCalcMinRequiredTxRelayFee tests the calcMinRequiredTxRelayFee API.
func TestCalcMinRequiredTxRelayFee(t *testing.T) {
	tests := []struct {
		name     string         // test description.
		size     int64          // Transaction size in bytes.
		relayFee btcutil.Amount // minimum relay transaction fee.
		want     int64          // Expected fee.
	}{
		{
			// Ensure combination of size and fee that are less than 1000
			// produce a non-zero fee.
			"250 bytes with relay fee of 3",
			250,
			3,
			3,
		},
		{
			// 0.001 DOGE/kB.
			"100 bytes with default minimum relay fee",
			100,
			DefaultMinRelayTxFee,
			10000,
		},
		{
			"max standard tx size with default minimum relay fee",
			maxStandardTxWeight / 4,
			DefaultMinRelayTxFee,
			10000000,
		},
		{
			"max standard tx size with max satoshi relay fee",
			maxStandardTxWeight / 4,
			btcutil.MaxSatoshi,
			btcutil.MaxSatoshi,
		},
		{
			"1500 bytes with 5000 relay fee",
			1500,
			5000,
			7500,
		},
		{
			"1500 bytes with 3000 relay fee",
			1500,
			3000,
			4500,
		},
		{
			"782 bytes with 5000 relay fee",
			782,
			5000,
			3910,
		},
		{
			"782 bytes with 3000 relay fee",
			782,
			3000,
			2346,
		},
		{
			"782 bytes with 2550 relay fee",
			782,
			2550,
			1994,
		},
	}

	for _, test := range tests {
		got := calcMinRequiredTxRelayFee(test.size, test.relayFee)
		if got != test.want {
			t.Errorf("TestCalcMinRequiredTxRelayFee test '%s' "+
				"failed: got %v want %v", test.name, got,
				test.want)
			continue
		}
	}
}

// TestCheckPkScriptStandard tests the checkPkScriptStandard API.
func TestCheckPkScriptStandard(t *testing.T) {
	var pubKeys [][]byte
	for i := 0; i < 4; i++ {
		pk, err := btcec.NewPrivateKey()
		if err != nil {
			t.Fatalf("TestCheckPkScriptStandard NewPrivateKey failed: %v",
				err)
			return
		}
		pubKeys = append(pubKeys, pk.PubKey().SerializeCompressed())
	}

	tests := []struct {
		name       string // test description.
		script     *txscript.ScriptBuilder
		isStandard bool
	}{
		{
			"key1 and key2",
			txscript.NewScriptBuilder().AddOp(txscript.OP_2).
				AddData(pubKeys[0]).AddData(pubKeys[1]).
				AddOp(txscript.OP_2).AddOp(txscript.OP_CHECKMULTISIG),
			true,
		},
		{
			"key1 or key2",
			txscript.NewScriptBuilder().AddOp(txscript.OP_1).
				AddData(pubKeys[0]).AddData(pubKeys[1]).
				AddOp(txscript.OP_2).AddOp(txscript.OP_CHECKMULTISIG),
			true,
		},
		{
			"escrow",
			txscript.NewScriptBuilder().AddOp(txscript.OP_2).
				AddData(pubKeys[0]).AddData(pubKeys[1]).
				AddData(pubKeys[2]).
				AddOp(txscript.OP_3).AddOp(txscript.OP_CHECKMULTISIG),
			true,
		},
		{
			"one of four",
			txscript.NewScriptBuilder().AddOp(txscript.OP_1).
				AddData(pubKeys[0]).AddData(pubKeys[1]).
				AddData(pubKeys[2]).AddData(pubKeys[3]).
				AddOp(txscript.OP_4).AddOp(txscript.OP_CHECKMULTISIG),
			false,
		},
		{
			"malformed1",
			txscript.NewScriptBuilder().AddOp(txscript.OP_3).
				AddData(pubKeys[0]).AddData(pubKeys[1]).
				AddOp(txscript.OP_2).AddOp(txscript.OP_CHECKMULTISIG),
			false,
		},
		{
			"malformed2",
			txscript.NewScriptBuilder().AddOp(txscript.OP_2).
				AddData(pubKeys[0]).AddData(pubKeys[1]).
				AddOp(txscript.OP_3).AddOp(txscript.OP_CHECKMULTISIG),
			false,
		},
		{
			"malformed3",
			txscript.NewScriptBuilder().AddOp(txscript.OP_0).
				AddData(pubKeys[0]).AddData(pubKeys[1]).
				AddOp(txscript.OP_2).AddOp(txscript.OP_CHECKMULTISIG),
			false,
		},
		{
			"malformed4",
			txscript.NewScriptBuilder().AddOp(txscript.OP_1).
				AddData(pubKeys[0]).AddData(pubKeys[1]).
				AddOp(txscript.OP_0).AddOp(txscript.OP_CHECKMULTISIG),
			false,
		},
		{
			"malformed5",
			txscript.NewScriptBuilder().AddOp(txscript.OP_1).
				AddData(pubKeys[0]).AddData(pubKeys[1]).
				AddOp(txscript.OP_CHECKMULTISIG),
			false,
		},
		{
			"malformed6",
			txscript.NewScriptBuilder().AddOp(txscript.OP_1).
				AddData(pubKeys[0]).AddData(pubKeys[1]),
			false,
		},
	}

	for _, test := range tests {
		script, err := test.script.Script()
		if err != nil {
			t.Fatalf("TestCheckPkScriptStandard test '%s' "+
				"failed: %v", test.name, err)
		}
		scriptClass := txscript.GetScriptClass(script)
		got := checkPkScriptStandard(script, scriptClass)
		if (test.isStandard && got != nil) ||
			(!test.isStandard && got == nil) {

			t.Fatalf("TestCheckPkScriptStandard test '%s' failed",
				test.name)
			return
		}
	}
}

// TestDust tests the IsDust API.
func TestDust(t *testing.T) {
	p2pkh := []byte{0x76, 0xa9, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x88, 0xac}
	opReturn := []byte{txscript.OP_RETURN, 0x01, 0x42}

	tests := []struct {
		name      string
		txOut     wire.TxOut
		dustLimit btcutil.Amount
		isDust    bool
	}{
		{"zero value with zero limit", wire.TxOut{Value: 0, PkScript: p2pkh}, 0, false},
		{"zero value", wire.TxOut{Value: 0, PkScript: p2pkh}, DefaultHardDustLimit, true},
		{"one koinu below hard limit", wire.TxOut{Value: 99999, PkScript: p2pkh}, DefaultHardDustLimit, true},
		{"exactly hard limit", wire.TxOut{Value: 100000, PkScript: p2pkh}, DefaultHardDustLimit, false},
		{"one koinu below soft limit", wire.TxOut{Value: 999999, PkScript: p2pkh}, DefaultDustLimit, true},
		{"exactly soft limit", wire.TxOut{Value: 1000000, PkScript: p2pkh}, DefaultDustLimit, false},
		{"MAX_MONEY", wire.TxOut{Value: btcutil.MaxSatoshi, PkScript: p2pkh}, DefaultDustLimit, false},
		{"maximum int64 value", wire.TxOut{Value: 1<<63 - 1, PkScript: p2pkh}, 1<<63 - 1, false},
		// As in Dogecoin Core, unspendable outputs are never dust.
		{"zero value OP_RETURN", wire.TxOut{Value: 0, PkScript: opReturn}, DefaultDustLimit, false},
	}
	for _, test := range tests {
		if got := IsDust(&test.txOut, test.dustLimit); got != test.isDust {
			t.Errorf("Dust test '%s' failed: want %v got %v",
				test.name, test.isDust, got)
		}
	}
}

// TestCalcDustFee checks Dogecoin's soft dust surcharge: the dust limit is
// added once for every spendable output below it.
func TestCalcDustFee(t *testing.T) {
	p2pkh := []byte{0x76, 0xa9, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x88, 0xac}
	opReturn := []byte{txscript.OP_RETURN, 0x01, 0x42}

	tests := []struct {
		name   string
		values []int64
		script []byte
		want   int64
	}{
		{"no dust", []int64{1000000, 5e8}, p2pkh, 0},
		{"one dust output", []int64{999999, 5e8}, p2pkh, 1000000},
		{"three dust outputs", []int64{1, 500000, 999999, 5e8}, p2pkh, 3000000},
		{"OP_RETURN is not dust", []int64{0}, opReturn, 0},
	}
	for _, test := range tests {
		tx := wire.NewMsgTx(1)
		for _, v := range test.values {
			tx.AddTxOut(wire.NewTxOut(v, test.script))
		}
		if got := calcDustFee(tx, DefaultDustLimit); got != test.want {
			t.Errorf("%s: dust fee %d, want %d", test.name, got, test.want)
		}
	}

	// The surcharge is clamped to MAX_MONEY rather than overflowing.
	tx := wire.NewMsgTx(1)
	for i := 0; i < 3; i++ {
		tx.AddTxOut(wire.NewTxOut(0, p2pkh))
	}
	if got := calcDustFee(tx, btcutil.MaxSatoshi); got != btcutil.MaxSatoshi {
		t.Errorf("huge dust limit: dust fee %d, want MaxSatoshi", got)
	}
}

// TestCheckTransactionStandard tests the CheckTransactionStandard API.
func TestCheckTransactionStandard(t *testing.T) {
	// Create some dummy, but otherwise standard, data for transactions.
	prevOutHash, err := chainhash.NewHashFromStr("01")
	if err != nil {
		t.Fatalf("NewShaHashFromStr: unexpected error: %v", err)
	}
	dummyPrevOut := wire.OutPoint{Hash: *prevOutHash, Index: 1}
	dummySigScript := bytes.Repeat([]byte{0x00}, 65)
	dummyTxIn := wire.TxIn{
		PreviousOutPoint: dummyPrevOut,
		SignatureScript:  dummySigScript,
		Sequence:         wire.MaxTxInSequenceNum,
	}
	addrHash := [20]byte{0x01}
	addr, err := btcutil.NewAddressPubKeyHash(addrHash[:],
		&chaincfg.TestNet3Params)
	if err != nil {
		t.Fatalf("NewAddressPubKeyHash: unexpected error: %v", err)
	}
	dummyPkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		t.Fatalf("PayToAddrScript: unexpected error: %v", err)
	}
	dummyTxOut := wire.TxOut{
		Value:    100000000, // 1 BTC
		PkScript: dummyPkScript,
	}

	tests := []struct {
		name       string
		tx         wire.MsgTx
		height     int32
		isStandard bool
		code       wire.RejectCode
	}{
		{
			name: "Typical pay-to-pubkey-hash transaction",
			tx: wire.MsgTx{
				Version:  1,
				TxIn:     []*wire.TxIn{&dummyTxIn},
				TxOut:    []*wire.TxOut{&dummyTxOut},
				LockTime: 0,
			},
			height:     300000,
			isStandard: true,
		},
		{
			name: "Transaction version too high",
			tx: wire.MsgTx{
				Version:  wire.TxVersion + 1,
				TxIn:     []*wire.TxIn{&dummyTxIn},
				TxOut:    []*wire.TxOut{&dummyTxOut},
				LockTime: 0,
			},
			height:     300000,
			isStandard: false,
			code:       wire.RejectNonstandard,
		},
		{
			name: "Transaction is not finalized",
			tx: wire.MsgTx{
				Version: 1,
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: dummyPrevOut,
					SignatureScript:  dummySigScript,
					Sequence:         0,
				}},
				TxOut:    []*wire.TxOut{&dummyTxOut},
				LockTime: 300001,
			},
			height:     300000,
			isStandard: false,
			code:       wire.RejectNonstandard,
		},
		{
			name: "Transaction size is too large",
			tx: wire.MsgTx{
				Version: 1,
				TxIn:    []*wire.TxIn{&dummyTxIn},
				TxOut: []*wire.TxOut{{
					Value: 0,
					PkScript: bytes.Repeat([]byte{0x00},
						(maxStandardTxWeight/4)+1),
				}},
				LockTime: 0,
			},
			height:     300000,
			isStandard: false,
			code:       wire.RejectNonstandard,
		},
		{
			name: "Signature script size is too large",
			tx: wire.MsgTx{
				Version: 1,
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: dummyPrevOut,
					SignatureScript: bytes.Repeat([]byte{0x00},
						maxStandardSigScriptSize+1),
					Sequence: wire.MaxTxInSequenceNum,
				}},
				TxOut:    []*wire.TxOut{&dummyTxOut},
				LockTime: 0,
			},
			height:     300000,
			isStandard: false,
			code:       wire.RejectNonstandard,
		},
		{
			name: "Signature script that does more than push data",
			tx: wire.MsgTx{
				Version: 1,
				TxIn: []*wire.TxIn{{
					PreviousOutPoint: dummyPrevOut,
					SignatureScript: []byte{
						txscript.OP_CHECKSIGVERIFY},
					Sequence: wire.MaxTxInSequenceNum,
				}},
				TxOut:    []*wire.TxOut{&dummyTxOut},
				LockTime: 0,
			},
			height:     300000,
			isStandard: false,
			code:       wire.RejectNonstandard,
		},
		{
			name: "Valid but non standard public key script",
			tx: wire.MsgTx{
				Version: 1,
				TxIn:    []*wire.TxIn{&dummyTxIn},
				TxOut: []*wire.TxOut{{
					Value:    100000000,
					PkScript: []byte{txscript.OP_TRUE},
				}},
				LockTime: 0,
			},
			height:     300000,
			isStandard: false,
			code:       wire.RejectNonstandard,
		},
		{
			name: "More than one nulldata output",
			tx: wire.MsgTx{
				Version: 1,
				TxIn:    []*wire.TxIn{&dummyTxIn},
				TxOut: []*wire.TxOut{{
					Value:    0,
					PkScript: []byte{txscript.OP_RETURN},
				}, {
					Value:    0,
					PkScript: []byte{txscript.OP_RETURN},
				}},
				LockTime: 0,
			},
			height:     300000,
			isStandard: false,
			code:       wire.RejectNonstandard,
		},
		{
			name: "Dust output",
			tx: wire.MsgTx{
				Version: 1,
				TxIn:    []*wire.TxIn{&dummyTxIn},
				TxOut: []*wire.TxOut{{
					Value:    0,
					PkScript: dummyPkScript,
				}},
				LockTime: 0,
			},
			height:     300000,
			isStandard: false,
			code:       wire.RejectDust,
		},
		{
			name: "One nulldata output with 0 amount (standard)",
			tx: wire.MsgTx{
				Version: 1,
				TxIn:    []*wire.TxIn{&dummyTxIn},
				TxOut: []*wire.TxOut{{
					Value:    0,
					PkScript: []byte{txscript.OP_RETURN},
				}},
				LockTime: 0,
			},
			height:     300000,
			isStandard: true,
		},
	}

	pastMedianTime := time.Now()
	for _, test := range tests {
		// Ensure standardness is as expected.
		err := CheckTransactionStandard(btcutil.NewTx(&test.tx),
			test.height, pastMedianTime, DefaultMinRelayTxFee, 1)
		if err == nil && test.isStandard {
			// Test passes since function returned standard for a
			// transaction which is intended to be standard.
			continue
		}
		if err == nil && !test.isStandard {
			t.Errorf("CheckTransactionStandard (%s): standard when "+
				"it should not be", test.name)
			continue
		}
		if err != nil && test.isStandard {
			t.Errorf("CheckTransactionStandard (%s): nonstandard "+
				"when it should not be: %v", test.name, err)
			continue
		}

		// Ensure error type is a TxRuleError inside of a RuleError.
		rerr, ok := err.(RuleError)
		if !ok {
			t.Errorf("CheckTransactionStandard (%s): unexpected "+
				"error type - got %T", test.name, err)
			continue
		}
		txrerr, ok := rerr.Err.(TxRuleError)
		if !ok {
			t.Errorf("CheckTransactionStandard (%s): unexpected "+
				"error type - got %T", test.name, rerr.Err)
			continue
		}

		// Ensure the reject code is the expected one.
		if txrerr.RejectCode != test.code {
			t.Errorf("CheckTransactionStandard (%s): unexpected "+
				"error code - got %v, want %v", test.name,
				txrerr.RejectCode, test.code)
			continue
		}
	}
}
