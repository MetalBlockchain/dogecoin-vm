package main

// The anchor lineage. Every transaction the signers sign on a chain spends
// that chain's anchor first, so any two of them either conflict (at most
// one confirms) or one descends from the other. That holds only if every
// honest signer agrees which coin is the anchor, so the anchor is never
// "some coin the signers made" or "the oldest coin": it is the tip of one
// line of coins, the same for every signer that reads the same chain.
//
// The line starts at the root: the oldest output ever paid to script (the
// peg address on Dogecoin, the reserve on DogecoinVM) by a transaction with
// at least rootDepth confirmations, read from history, spent or not. It
// moves only along confirmed spends: when a transaction in a block spends
// the current coin, the line moves to that transaction's last output paying
// script (its change; every transaction the signers build pays change back
// to script, at least pegDust). A transaction in the mempool doesn't move
// it. The tip is the coin no confirmed transaction spends.
//
// Coins others pay to script (deposits, contributions, peg-outs into the
// reserve) never become the anchor unless one is the root: they are spent,
// beside the anchor, by transactions that need them.
//
// If a confirmed transaction spends the line's coin and pays nothing back
// to script (no transaction built today does; an old one might have), the
// line starts again at the oldest coin paid to script, with rootDepth
// confirmations, in a later block. Both are read from blocks, so every
// signer with the chain's history finds the same.

import (
	"bytes"
	"fmt"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

type lineage struct {
	script    []byte
	rootDepth int64
	// spentBy maps an output to the confirmed transaction that spends it.
	spentBy map[wire.OutPoint]chainTx
	// coins is every output paid to script by a confirmed transaction.
	coins []utxo
}

// newLineage reads the line of coins at script from txs, a chain's history
// of script (confirmed or not; only confirmed transactions count).
func newLineage(txs []chainTx, script []byte, rootDepth int64) *lineage {
	l := &lineage{script: script, rootDepth: rootDepth, spentBy: map[wire.OutPoint]chainTx{}}
	for _, t := range txs {
		if t.confirmations <= 0 {
			continue
		}
		for _, in := range t.tx.TxIn {
			l.spentBy[in.PreviousOutPoint] = t
		}
		hash := t.tx.TxHash()
		for i, out := range t.tx.TxOut {
			if bytes.Equal(out.PkScript, script) {
				l.coins = append(l.coins, utxo{outPoint: wire.OutPoint{Hash: hash, Index: uint32(i)},
					value: out.Value, pkScript: out.PkScript, confirmations: t.confirmations})
			}
		}
	}
	return l
}

// root is the oldest coin at script with at least rootDepth confirmations,
// and, if before > 0, fewer than before (in a later block than one with
// before confirmations); nil if there is none.
func (l *lineage) root(before int64) *utxo {
	var eligible []utxo
	for _, u := range l.coins {
		if u.confirmations >= l.rootDepth && (before <= 0 || u.confirmations < before) {
			eligible = append(eligible, u)
		}
	}
	return oldestCoin(eligible)
}

// step is the coin after cur on the line, and whether there is one (a
// confirmed transaction spends cur). It fails if one spends cur, pays
// nothing back to script, and no coin at script is deep enough to start
// the line again.
func (l *lineage) step(cur wire.OutPoint) (wire.OutPoint, bool, error) {
	t, spent := l.spentBy[cur]
	if !spent {
		return cur, false, nil
	}
	hash := t.tx.TxHash()
	for i := len(t.tx.TxOut) - 1; i >= 0; i-- {
		if bytes.Equal(t.tx.TxOut[i].PkScript, l.script) {
			return wire.OutPoint{Hash: hash, Index: uint32(i)}, true, nil
		}
	}
	if r := l.root(t.confirmations); r != nil {
		return r.outPoint, true, nil
	}
	return cur, false, fmt.Errorf("the anchor %v was spent by %v, which pays nothing back to the peg, and no later coin there has %d confirmations yet",
		cur, hash, l.rootDepth)
}

// tip follows the line from its root to the coin no confirmed transaction
// spends. It returns that coin, the root, and whether the line has moved
// from its root at all. ok is false while there is no root yet.
func (l *lineage) tip() (tip, root wire.OutPoint, moved, ok bool, err error) {
	r := l.root(0)
	if r == nil {
		return wire.OutPoint{}, wire.OutPoint{}, false, false, nil
	}
	cur := r.outPoint
	for range len(l.coins) + 1 {
		next, more, err := l.step(cur)
		if err != nil {
			return cur, r.outPoint, moved, false, err
		}
		if !more {
			return cur, r.outPoint, moved, true, nil
		}
		cur, moved = next, true
	}
	return cur, r.outPoint, moved, false, fmt.Errorf("the anchor's line at %x is longer than its coins", l.script)
}

// anchorIn is the tip of the line, as one of coins (the confirmed coins at
// script this node lists, and those an unconfirmed transaction spends);
// nil if there is none, or the tip isn't among them (a wallet missing
// history: then nothing is signed).
func (l *lineage) anchorIn(coins []utxo) *utxo {
	tip, _, _, ok, err := l.tip()
	if err != nil || !ok {
		return nil
	}
	for _, u := range coins {
		if u.outPoint == tip {
			c := u
			return &c
		}
	}
	return nil
}
