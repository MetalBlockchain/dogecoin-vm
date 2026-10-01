package main

import (
	"strings"
	"testing"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
	"github.com/stretchr/testify/require"
)

func TestActivityAnnouncesEachEventOnce(t *testing.T) {
	op := wire.OutPoint{Hash: chainhash.Hash{1}, Index: 0}
	old := wire.OutPoint{Hash: chainhash.Hash{9}, Index: 0}
	s := &pegState{
		deposits: []deposit{{outPoint: old, value: 1000, valid: true, confirmations: 50}},
		released: map[wire.OutPoint]chainhash.Hash{old: {8}},
		refunded: map[wire.OutPoint]chainhash.Hash{},
		paid:     map[chainhash.Hash]chainhash.Hash{},
	}
	a := newActivity()
	// The first pass only learns what is already there.
	require.Empty(t, a.events(s, 3))

	s.deposits = append(s.deposits, deposit{outPoint: op, value: 50000, valid: true})
	lines := a.events(s, 4)
	require.Len(t, lines, 2)
	require.Contains(t, lines[0], "Deposit received: 0.00050000 DOGE")
	require.Contains(t, lines[0], "unconfirmed")
	require.Equal(t, "A new deposit address registered (4 in all)", lines[1])
	require.Empty(t, a.events(s, 4), "nothing new, nothing said")

	s.released[op] = chainhash.Hash{2}
	pegOutID := chainhash.Hash{3}
	s.pegOuts = []pegOut{{txid: pegOutID, value: 20000, valid: true}}
	lines = a.events(s, 4)
	require.Len(t, lines, 2)
	require.True(t, strings.HasPrefix(lines[0], "Deposit credited: 0.00050000 DOGE on DogecoinVM"))
	require.True(t, strings.HasPrefix(lines[1], "Withdrawal requested: 0.00020000 DOGE"))

	s.paid[pegOutID] = chainhash.Hash{4}
	lines = a.events(s, 4)
	require.Len(t, lines, 1)
	require.True(t, strings.HasPrefix(lines[0], "Withdrawal paid: 0.00020000 DOGE on Dogecoin, tx "))

	held := wire.OutPoint{Hash: chainhash.Hash{5}, Index: 1}
	s.held = []deposit{{outPoint: held, value: 1, confirmations: 2}}
	lines = a.events(s, 4)
	require.Len(t, lines, 1)
	require.Contains(t, lines[0], "2 confirmations, held: no destination, or outside the deposit limits")
}
