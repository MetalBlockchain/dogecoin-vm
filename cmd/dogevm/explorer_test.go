package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// TestExplorerLabelsMaintenance checks the explorer names the bridge's own
// housekeeping, a fence (DVMZ) or a cancellation (DVMX) of the reserve to
// itself, as maintenance, and only when the reserve is spent: anyone can
// write the tag.
func TestExplorerLabelsMaintenance(t *testing.T) {
	require := require.New(t)
	signers, err := newSignerSet(2, 3)
	require.NoError(err)
	vmParams, _ := dogevmParams("testnet")
	srv := &server{b: &bridge{signers: signers, vmParams: vmParams, dogeParams: &dogecoinTestNet}}
	reserve := signers.pkScript()

	toSelf := func(tag []byte, payTo []byte) *wire.MsgTx {
		tx := wire.NewMsgTx(1)
		first := wire.OutPoint{Hash: [32]byte{1}, Index: 1}
		tx.AddTxIn(wire.NewTxIn(&first, nil, nil))
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: [32]byte{2}}, nil, nil))
		tx.AddTxOut(wire.NewTxOut(5*koinuPerDoge, payTo))
		tx.AddTxOut(nullData(encodeOutPointTag(tag, first)))
		return tx
	}
	describe := func(tx *wire.MsgTx, spendsReserve bool) *txView {
		v := &txView{}
		srv.describe(v, tx, spendsReserve)
		return v
	}

	// The v0.2.0 clean start: a fence of the reserve to itself.
	v := describe(toSelf(tagFence, reserve), true)
	require.Equal("maintenance", v.Kind)
	require.Contains(v.Label, "Bridge maintenance (fence)")
	require.Contains(v.Label, "operating capital")

	v = describe(toSelf(tagCancel, reserve), true)
	require.Equal("maintenance", v.Kind)
	require.Contains(v.Label, "Bridge maintenance (cancel)")

	// The same tag on someone else's coins is not the bridge's.
	v = describe(toSelf(tagFence, reserve), false)
	require.Equal("transfer", v.Kind)
	require.Contains(v.Label, "unverified bridge message")

	// Nor is a reserve spend that pays anyone else.
	other := destination{kind: destP2PKH, hash: [20]byte{7}}.pkScript()
	v = describe(toSelf(tagFence, other), true)
	require.Equal("transfer", v.Kind)
	tx := toSelf(tagFence, reserve)
	tx.AddTxOut(wire.NewTxOut(koinuPerDoge, other))
	v = describe(tx, true)
	require.Equal("transfer", v.Kind)

	// Other tags on a reserve-to-itself spend stay unverified.
	v = describe(toSelf(tagRefund, reserve), true)
	require.Equal("transfer", v.Kind)
}
