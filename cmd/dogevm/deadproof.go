package main

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// Clearing an unsettled transaction the wallet dropped (Round 7, W8).
//
// A transaction the wallet stops listing comes back from the unsettled file
// as conflicted (deadhistory.go), and its outputs stay recorded until it is
// settled: until it can never be mined. Its disappearance proves nothing.
// What does is a transaction spending one of its inputs in a block
// rootDepth deep: the two can't both be in the chain, and a reorganisation
// shallower than rootDepth can't take the spender out. Its depth is read
// fresh on every history read, never remembered: a reorganisation can make
// it shallower again.
//
// That spender usually pays someone else (a payer double-spending a
// deposit), so this node's wallet doesn't list it, and Dogecoin Core 1.14
// without a transaction index can't look up what spent an output. So the
// evidence comes from one of two places:
//   - the wallet, when it lists a transaction in a block that spends one of
//     the dropped transaction's inputs;
//   - proofs an operator records (`dogevm unsettled prove`): the spender's
//     bytes and the hash of the block holding it, kept side by side. Nothing in it is taken on
//     trust: on every read this node checks the bytes spend one of the
//     dropped transaction's inputs, that the block is on its main chain,
//     how deep it is, and that the block lists the spender. A wrong proof
//     clears nothing.

// conflictProof is an operator-recorded spender of a dropped transaction's
// input, and the block holding it.
type conflictProof struct {
	Spender string `json:"spender"` // the spending transaction, hex
	Block   string `json:"block"`   // the hash of the block holding it
}

// spendsInputOf reports whether spender spends one of dropped's inputs and
// is not dropped itself. A coinbase's input names no coin (the null
// outpoint every coinbase shares), so it is never evidence (Round 8, X3):
// two unrelated coinbases would otherwise "conflict", and a dropped
// coinbase stays unsettled, failing closed.
func spendsInputOf(spender, dropped *wire.MsgTx) bool {
	if spender.TxHash() == dropped.TxHash() {
		return false
	}
	ins := map[wire.OutPoint]bool{}
	for _, in := range dropped.TxIn {
		if isNullOutPoint(in.PreviousOutPoint) {
			continue
		}
		ins[in.PreviousOutPoint] = true
	}
	for _, in := range spender.TxIn {
		if ins[in.PreviousOutPoint] {
			return true
		}
	}
	return false
}

// proofDepth is how deep, on this node's main chain, the block a proof
// names is, if it holds a transaction spending one of dropped's inputs (a
// conflict: dropped can never be mined); 0 if it isn't on the main chain.
// Only a conflict counts (Round 10, Z2): a proof that one of dropped's own
// OUTPUTS is spent would say dropped was mined after all, and then the
// customer claim it carried has to come back into the books with it; this
// node can't rebuild that from a proof, so that case fails closed (rescan
// or import the transaction, RUNBOOK "Cutover").
func (c *dogeChain) proofDepth(dropped *wire.MsgTx, p conflictProof) (int64, error) {
	spender, err := decodeTx(p.Spender)
	if err != nil {
		return 0, fmt.Errorf("the proof's spender: %w", err)
	}
	if !spendsInputOf(spender, dropped) {
		return 0, errors.New("the proof's spender doesn't spend any of the dropped transaction's inputs")
	}
	var block struct {
		Confirmations int64    `json:"confirmations"`
		Tx            []string `json:"tx"`
	}
	if err := c.rpc.call(&block, "getblock", p.Block, true); err != nil {
		return 0, fmt.Errorf("the proof's block %s: %w", p.Block, err)
	}
	if block.Confirmations < 1 {
		return 0, nil // not on the main chain (any more)
	}
	id := spender.TxHash().String()
	for _, txid := range block.Tx {
		if strings.EqualFold(txid, id) {
			return block.Confirmations, nil
		}
	}
	return 0, fmt.Errorf("block %s doesn't hold the proof's spender %s", p.Block, id)
}

// walletConflictDepth is the depth of the deepest transaction the wallet
// lists in a block that spends one of dropped's inputs; 0 if none.
func (c *dogeChain) walletConflictDepth(tip int64, dropped *wire.MsgTx, entries map[string]historyEntry) (int64, error) {
	var deepest int64
	for txid, e := range entries {
		if e.height <= 0 || e.conflicted {
			continue
		}
		tx, err := c.walletTx(txid)
		if err != nil {
			return 0, err
		}
		if spendsInputOf(tx, dropped) {
			deepest = max(deepest, tip-e.height+1)
		}
	}
	return deepest, nil
}

// droppedDepth is the current depth of the best evidence that dropped can
// never be mined: the deepest of the wallet's and every recorded proof's,
// each checked now. A proof that doesn't check out counts for nothing, and
// doesn't stop the read: the transaction just stays unsettled.
func (c *dogeChain) droppedDepth(tip int64, dropped *wire.MsgTx, entries map[string]historyEntry, proofs []conflictProof) (int64, error) {
	depth, err := c.walletConflictDepth(tip, dropped, entries)
	if err != nil {
		return 0, err
	}
	for _, p := range proofs {
		if pd, err := c.proofDepth(dropped, p); err == nil {
			depth = max(depth, pd)
		}
	}
	return depth, nil
}

// addProof records p for the unsettled transaction txid, beside any
// recorded before (Round 10, Z3): a later proof never replaces an earlier
// one, so a mistaken second proof can't un-clear what a good one cleared.
// Every one is checked again on every read.
func (d *deadHistory) addProof(txid string, p conflictProof) error {
	unlock, err := d.lock()
	if err != nil {
		return err
	}
	defer unlock()
	records, err := d.read()
	if err != nil {
		return err
	}
	rec, ok := records[txid]
	if !ok {
		return fmt.Errorf("%s has no unsettled transaction %s", d.path, txid)
	}
	for _, q := range rec.proofs() {
		if q == p {
			return nil
		}
	}
	rec.Proofs = append(rec.proofs(), p)
	rec.Proof = nil
	records[txid] = rec
	return d.write(records)
}

// isNullOutPoint reports whether op is a coinbase input's: no coin at all.
func isNullOutPoint(op wire.OutPoint) bool {
	return op.Index == wire.MaxPrevOutIndex && op.Hash == (chainhash.Hash{})
}

// validHex reports whether s is non-empty hex.
func validHex(s string) bool {
	_, err := hex.DecodeString(s)
	return s != "" && err == nil
}
