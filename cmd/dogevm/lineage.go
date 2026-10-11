package main

// The anchor lineage. Every transaction the signers sign on a chain spends
// that chain's anchor first, so any two of them either conflict (at most
// one confirms) or one descends from the other. That holds only if every
// honest signer agrees which coin is the anchor, so the anchor is never
// "some coin the signers made" or "the oldest coin": it is the tip of one
// line of coins, the same for every signer that reads the same chain.
//
// The line starts at the root: of the outputs ever paid to script (the peg
// address on Dogecoin, the reserve on DogecoinVM) by a transaction with at
// least rootDepth confirmations, read from history, spent or not, the
// oldest one the signers have spent (only they can spend script), or, while
// they have spent none, the oldest of all. Ties go to the lower outpoint.
// Preferring a spent coin means a payment someone shows a node late, in an
// old block, can't take the line over once it has begun: nobody has spent
// it. It moves only along confirmed spends: when a transaction in a block
// spends the current coin, the line moves to that transaction's last output
// paying script (its change; every transaction the signers build pays
// change back to script, at least pegDust). A transaction in the mempool
// doesn't move it. The tip is the coin no confirmed transaction spends.
// The same rule as BTCVM's and LTCVM's.
//
// Coins others pay to script (deposits, contributions, peg-outs into the
// reserve) never become the anchor unless one is the root: they are spent,
// beside the anchor, by transactions that need them.
//
// If a confirmed transaction spends the line's coin and pays nothing back
// to script (no transaction built today does; an old one might have), the
// line, as read from history, starts again at a root chosen the same way
// among coins in later blocks. A signer's log does not follow that restart
// (anchorMoved refuses it, lineage.next): the coin its last signature spent
// led nowhere, and moving on to another is what an operator decides, by
// fencing (fence.go), not what a signer assumes.

import (
	"bytes"
	"errors"
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

// root is the line's start among coins at script with at least rootDepth
// confirmations and, if before > 0, fewer than before (in a later block
// than one with before confirmations): the oldest the signers have spent,
// else the oldest of all; nil if there is none.
func (l *lineage) root(before int64) *utxo {
	var spent, unspent []utxo
	for _, u := range l.coins {
		if u.confirmations < l.rootDepth || before > 0 && u.confirmations >= before {
			continue
		}
		if _, ok := l.spentBy[u.outPoint]; ok {
			spent = append(spent, u)
		} else {
			unspent = append(unspent, u)
		}
	}
	if len(spent) > 0 {
		return oldestCoin(spent)
	}
	return oldestCoin(unspent)
}

// errLineEnded: a confirmed transaction spent the coin and paid nothing
// back to script.
var errLineEnded = errors.New("the line ended")

// next is the coin after cur on the line without a restart, and whether
// there is one (a confirmed transaction spends cur): the spending
// transaction's last output to script. If that transaction pays nothing
// back, it fails with errLineEnded.
func (l *lineage) next(cur wire.OutPoint) (wire.OutPoint, bool, error) {
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
	return cur, false, fmt.Errorf("%w: the anchor %v was spent by %v, which pays nothing back to the peg", errLineEnded, cur, hash)
}

// step is the coin after cur on the line, and whether there is one (a
// confirmed transaction spends cur). It fails if one spends cur, pays
// nothing back to script, and no coin at script is deep enough to start
// the line again.
func (l *lineage) step(cur wire.OutPoint) (wire.OutPoint, bool, error) {
	next, more, err := l.next(cur)
	if !errors.Is(err, errLineEnded) {
		return next, more, err
	}
	t := l.spentBy[cur]
	hash := t.tx.TxHash()
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
