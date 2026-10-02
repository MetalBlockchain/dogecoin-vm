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
