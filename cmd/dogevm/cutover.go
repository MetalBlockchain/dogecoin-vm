package main

// Cutover and fences (docs/RUNBOOK.md, "Cutover"). A signature already
// given stays valid for as long as every coin its transaction spends is
// unspent. A log rebuilt from the chains (adopt) holds only what reached
// them, not a signature someone kept back; a log from software before this
// rule may hold signatures that spend any coin of the set (the live
// DogecoinVM signers signed for months without logs). Matching anchors
// today don't kill such a signature: it could be mined later beside a
// second one for the same withdrawal, signed from another coin.
//
// So such a signer goes through a cutover. Its log records, per chain,
// every coin a signature given before could spend (cutoverCoins): every
// output to the set's scripts in any transaction its wallet lists,
// confirmed, unconfirmed, or conflicting with one in a block, plus every
// input of every transaction in its log, less those already spent deep. It
// signs nothing but fences, on either chain, until every recorded coin is
// cleared: spent by a transaction rootDepth deep (DogecoinVM: in a block,
// final), or its funding transaction conflicted by one that deep. Nothing
// else clears a coin: its transaction's absence from a block or a mempool
// proves nothing, since it can still be mined. Clearing every coin one by one
// is what makes a multi-batch cutover safe: a later batch's spend must be
// deep too, not just the first fence's.
//
// A fence is a transaction of the set's coins to itself: the lineage's tip
// first, then every confirmed recorded coin with no confirmed spend, oldest
// first, all to one output at the set's own script (the peg address on
// Dogecoin, the reserve on DogecoinVM), tagged DVMZ with its first input.
// It pays no one, so it is signed even while paused. Its fee is the
// surplus's, so a signer signs one only inside a cutover, only if it spends
// a recorded coin with no confirmed spend, only while no other fence of
// this cutover is unconfirmed (a stuck Dogecoin one may be replaced, same
// inputs, one fence fee more, after -fence-bump-after) and, stricter than
// that, only while none is in a block short of its depth (it may finish
// the work by waiting). Then the signer's anchor is the lineage's tip on
// each chain and it signs as usual.
//
// adopt starts a cutover; so does a signer whose log is unproven (History
// "") on its first status or sign request; `signer-log cutover` starts a
// new one by hand. The coordinator proposes fences with
// `dogevm bridge ... -fence CHAIN`. Deposits are unaffected: what is owed is
// read from the deposits' transactions, not from their coins still being
// where they were paid (as with a held deposit a payout pooled).

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

const actionFence = "fence" // spend the set's coins on a chain to itself, in a cutover

var tagFence = []byte("DVMZ")

func encodeFence(anchor wire.OutPoint) []byte { return encodeOutPointTag(tagFence, anchor) }

// parseFence reads the outpoint a fence names (its first input).
func parseFence(tx *wire.MsgTx) (wire.OutPoint, bool) { return parseOutPointTag(tx, tagFence) }

// maxFenceInputs bounds one fence, so it stays a standard transaction; a
// set holding more needs several, one after another.
func maxFenceInputs(chain string) int {
	if chain == chainDogecoin {
		return 100
	}
	return 400
}

// maxFenceBytes bounds a fence's signed size (Round 7, W4). Both chains
// refuse to relay a transaction over 100,000 bytes (400,000 weight, with no
// witness data): a fence of 400 deposit inputs is over it, and no fee gets
// it relayed. fenceSize already counts every signature at its largest; the
// rest is margin.
const maxFenceBytes = 90_000

// trimToSize drops inputs from the end until the fence's signed size is
// within maxFenceBytes (the anchor, first, always stays).
func (b *bridge) trimToSize(s *pegState, chain string, inputs []utxo) ([]utxo, error) {
	for len(inputs) > 1 {
		size, err := b.fenceSizeOf(s, chain, inputs)
		if err != nil {
			return nil, err
		}
		if size <= maxFenceBytes {
			break
		}
		// Shrink in proportion, then one at a time near the limit.
		n := int(int64(len(inputs)) * maxFenceBytes / size)
		inputs = inputs[:max(1, min(n, len(inputs)-1))]
	}
	return inputs, nil
}

// fenceSize is a fence's size in bytes once signed, from the redeem script
// of each input and the number of signatures each needs: P2SH multisig
// inputs (OP_0, the signatures, the redeem script), one output to the set's
// script and the tag output.
func fenceSize(redeems [][]byte, required int) int64 {
	size := int64(4 + varIntLen(len(redeems)) + 1 + 4) // version, input count, output count, locktime
	for _, redeem := range redeems {
		script := 1 + required*(1+73) + pushLen(len(redeem)) + len(redeem)
		size += int64(36 + varIntLen(script) + script + 4)
	}
	size += 8 + 1 + 23         // to the set's P2SH script
	size += 8 + 1 + 2 + 4 + 36 // OP_RETURN with the tag and an outpoint
	return size
}

func varIntLen(n int) int {
	switch {
	case n < 0xfd:
		return 1
	case n <= 0xffff:
		return 3
	}
	return 5
}

func pushLen(n int) int {
	switch {
	case n < 76:
		return 1
	case n <= 0xff:
		return 2
	}
	return 3
}

// fenceFee is a fence's fee for its signed size. On Dogecoin: base (the
// -doge-fee) for each started 10 kB, at least once. On DogecoinVM, the
// rule all three bridges share: max(vmFee, ceil(size * vmFee / 250)).
func fenceFee(chain string, base, size int64) int64 {
	if chain == chainDogecoinVM {
		return max(base, (size*base+249)/250)
	}
	return base * max(1, (size+9_999)/10_000)
}

// cutoverChains are the chains a cutover covers, in order.
var cutoverChains = []string{chainDogecoin, chainDogecoinVM}

// chainView is one chain as a cutover reads it.
type chainView struct {
	txs        []chainTx // wallet history, confirmed or not
	conflicted []chainTx // conflicting with a transaction in a block; confirmations: minus its depth
	depth      int64     // confirmations that make a spend final
	isCoin     func(*wire.TxOut) bool
	// spentAt is the confirmations of the confirmed transaction spending an
	// output; conflictDepth the depth of what a conflicted transaction
	// conflicts with.
	spentAt       map[wire.OutPoint]int64
	conflictDepth map[chainhash.Hash]int64
	funding       map[chainhash.Hash]chainTx
}

func (s *pegState) view(chain string) *chainView {
	v := &chainView{txs: s.dogeTxs, conflicted: s.dogeConflicted, depth: 1,
		isCoin: func(out *wire.TxOut) bool { return s.redeemFor[string(out.PkScript)] != nil }}
	if chain == chainDogecoinVM {
		script := []byte(nil)
		if s.reserveLine != nil {
			script = s.reserveLine.script
		}
		v.txs, v.conflicted = s.vmTxs, nil
		v.isCoin = func(out *wire.TxOut) bool { return script != nil && string(out.PkScript) == string(script) }
	} else if s.pegLine != nil {
		v.depth = max(s.pegLine.rootDepth, 1)
	}
	v.spentAt = map[wire.OutPoint]int64{}
	v.conflictDepth = map[chainhash.Hash]int64{}
	v.funding = map[chainhash.Hash]chainTx{}
	for _, t := range v.txs {
		v.funding[t.tx.TxHash()] = t
		if t.confirmations <= 0 {
			continue
		}
		for _, in := range t.tx.TxIn {
			v.spentAt[in.PreviousOutPoint] = max(v.spentAt[in.PreviousOutPoint], t.confirmations)
		}
	}
	for _, t := range v.conflicted {
		hash := t.tx.TxHash()
		v.funding[hash] = t
		v.conflictDepth[hash] = max(v.conflictDepth[hash], -t.confirmations)
	}
	return v
}

// cleared reports whether op can no longer be spent by a signature given
// before: spent deep enough, or its funding transaction conflicted by one
// deep enough.
func (v *chainView) cleared(op wire.OutPoint) bool {
	return v.spentAt[op] >= v.depth || v.conflictDepth[op.Hash] >= v.depth
}

// cutoverCoins is what a cutover records on each chain (see the top of the
// file): every set coin the wallet's history (confirmed, unconfirmed or
// conflicted) shows, and every input of every transaction in l, that isn't
// already cleared. A logged input in neither chain's history is an error:
// rescan or register its address first.
func cutoverCoins(s *pegState, l *signingLog) (map[string][]string, error) {
	views := map[string]*chainView{}
	coins := map[string]map[wire.OutPoint]bool{}
	for _, chain := range cutoverChains {
		v := s.view(chain)
		views[chain] = v
		coins[chain] = map[wire.OutPoint]bool{}
		for _, t := range append(append([]chainTx{}, v.txs...), v.conflicted...) {
			hash := t.tx.TxHash()
			for i, out := range t.tx.TxOut {
				op := wire.OutPoint{Hash: hash, Index: uint32(i)}
				if v.isCoin(out) && !v.cleared(op) {
					coins[chain][op] = true
				}
			}
		}
	}
	for key, a := range l.Actions {
		for _, t := range a.Txs {
			for _, raw := range t.Inputs {
				op, err := parseOutPoint(raw)
				if err != nil {
					return nil, fmt.Errorf("the log's %s: %w", key, err)
				}
				chain := t.Chain
				if chain == "" {
					for _, c := range cutoverChains {
						if _, ok := views[c].funding[op.Hash]; ok {
							chain = c
							break
						}
					}
				}
				v := views[chain]
				if v == nil {
					return nil, fmt.Errorf("the log's %s spent %v, whose transaction is in neither chain's history: rescan, or register its deposit address, before a cutover", key, op)
				}
				if _, ok := v.funding[op.Hash]; !ok {
					return nil, fmt.Errorf("the log's %s spent %v, whose transaction isn't in %s's history: rescan, or register its deposit address, before a cutover", key, op, chain)
				}
				if !v.cleared(op) {
					coins[chain][op] = true
				}
			}
		}
	}
	out := map[string][]string{}
	for chain, set := range coins {
		list := make([]string, 0, len(set))
		for op := range set {
			list = append(list, op.String())
		}
		sort.Strings(list)
		out[chain] = list
	}
	return out, nil
}

// startCutover starts a new cutover: a new epoch, recording every coin a
// signature given before could spend, together with any still recorded.
func (c *cosigner) startCutover(s *pegState) error {
	if c.b.signers.indexOf(c.key.PubKey()) < 0 {
		return errors.New("this key is not in the signer set")
	}
	coins, err := cutoverCoins(s, c.log)
	if err != nil {
		return err
	}
	if c.log.Cutover == nil {
		c.log.Cutover = map[string][]string{}
	}
	for _, chain := range cutoverChains {
		set := map[string]bool{}
		for _, op := range append(c.log.Cutover[chain], coins[chain]...) {
			set[op] = true
		}
		list := make([]string, 0, len(set))
		for op := range set {
			list = append(list, op)
		}
		sort.Strings(list)
		c.log.Cutover[chain] = list
	}
	c.log.CutoverEpoch++
	c.log.CutoverStarted = time.Now().Unix()
	c.b.logf("cutover %d started: %d coin(s) on %s and %d on %s to clear", c.log.CutoverEpoch,
		len(c.log.Cutover[chainDogecoin]), chainDogecoin, len(c.log.Cutover[chainDogecoinVM]), chainDogecoinVM)
	return c.log.write()
}

// unproven reports whether the log can't show that every signature its key
// gave is dead or logged: it signs only fences until a cutover is done.
func (l *signingLog) unproven() bool { return l.History == "" || len(l.Cutover) > 0 }

// cutoverLeft is, per chain in the cutover, the recorded coins not yet
// cleared in s.
func (c *cosigner) cutoverLeft(s *pegState) map[string][]string {
	out := map[string][]string{}
	for chain, coins := range c.log.Cutover {
		v := s.view(chain)
		left := []string{}
		for _, raw := range coins {
			if op, err := parseOutPoint(raw); err != nil || !v.cleared(op) {
				left = append(left, raw)
			}
		}
		if len(left) > 0 { // only chains with coins left: {} when none (FENCE-NAMES.md)
			out[chain] = left
		}
	}
	return out
}

// cutoverPending is, per chain in the cutover, the recorded coins no
// confirmed transaction spends at all: what a fence still has to spend.
func (c *cosigner) cutoverPending(s *pegState) map[string][]string {
	out := map[string][]string{}
	for chain, coins := range c.log.Cutover {
		v := s.view(chain)
		pending := []string{}
		for _, raw := range coins {
			op, err := parseOutPoint(raw)
			if err != nil || v.spentAt[op] == 0 && !v.cleared(op) {
				pending = append(pending, raw)
			}
		}
		if len(pending) > 0 {
			out[chain] = pending
		}
	}
	return out
}

// advanceCutover starts a cutover for an unproven log that has none yet,
// and ends each chain's part of one whose coins are all cleared; when none
// is left, the log is fenced and its anchors are the lineages' tips. It
// reports whether the log changed.
func (c *cosigner) advanceCutover(s *pegState) (bool, error) {
	if c.log.History == "" && len(c.log.Cutover) == 0 {
		if c.b.signers.indexOf(c.key.PubKey()) < 0 {
			return false, nil
		}
		if err := c.startCutover(s); err != nil {
			return false, err
		}
	}
	if len(c.log.Cutover) == 0 {
		return false, nil
	}
	left := c.cutoverLeft(s)
	changed := false
	for chain := range c.log.Cutover {
		if len(left[chain]) == 0 {
			delete(c.log.Cutover, chain)
			changed = true
			c.b.logf("cutover %d on %s done: every recorded coin is cleared", c.log.CutoverEpoch, chain)
		}
	}
	if !changed {
		return false, nil
	}
	if len(c.log.Cutover) == 0 {
		c.log.Cutover = nil
		c.log.History = historyFenced
		t, err := tips(s)
		if err != nil {
			return true, err
		}
		if c.log.Anchors == nil {
			c.log.Anchors = map[string]string{}
		}
		for chain, tip := range t {
			c.log.Anchors[chain] = tip
		}
		c.b.logf("cutover %d done on both chains: anchors %v", c.log.CutoverEpoch, c.log.Anchors)
	}
	return true, c.log.write()
}

// cutoverGate refuses anything but a fence while the log is unproven, on
// either chain: an action has a transaction on each chain (a release on
// DogecoinVM, a refund on Dogecoin), so a new one on a cleared chain could
// confirm beside an old one kept back on the other.
func (c *cosigner) cutoverGate(s *pegState) error {
	if _, err := c.advanceCutover(s); err != nil {
		return err
	}
	if !c.log.unproven() {
		return nil
	}
	left := map[string]int{}
	for chain, coins := range c.cutoverLeft(s) {
		left[chain] = len(coins)
	}
	return fmt.Errorf("this signer is in a cutover (coins left per chain: %v): it signs only fences, on either chain, until every coin its set held is cleared (RUNBOOK.md, \"Cutover\")", left)
}

// isFenceOf reports whether t is a fence of this cutover: tagged as one,
// spending a recorded coin (only the signers can spend those).
func isFenceOf(t chainTx, recorded map[wire.OutPoint]bool) bool {
	if _, ok := parseFence(t.tx); !ok {
		return false
	}
	for _, in := range t.tx.TxIn {
		if recorded[in.PreviousOutPoint] {
			return true
		}
	}
	return false
}

// fenceState is a chain's fences of a cutover recording recorded: one
// unconfirmed (pending), and whether one is in a block short of depth.
func fenceState(s *pegState, chain string, recorded map[wire.OutPoint]bool) (pending *chainTx, shallow bool) {
	v := s.view(chain)
	for i := range v.txs {
		t := v.txs[i]
		if !isFenceOf(t, recorded) {
			continue
		}
		switch {
		case t.confirmations == 0:
			// The latest replacement: the one paying most.
			if pending == nil || fenceFeeOf(s, chain, t.tx) > fenceFeeOf(s, chain, pending.tx) {
				pending = &t
			}
		case t.confirmations < v.depth:
			shallow = true
		}
	}
	return pending, shallow
}

// fenceFeeOf is what a fence pays: its inputs, as s knows them, less its
// outputs; -1 if an input is unknown.
func fenceFeeOf(s *pegState, chain string, tx *wire.MsgTx) int64 {
	var in int64
	for _, txIn := range tx.TxIn {
		v, ok := coinValue(s, chain, txIn.PreviousOutPoint)
		if !ok {
			return -1
		}
		in += v
	}
	for _, out := range tx.TxOut {
		in -= out.Value
	}
	return in
}

// coinValue is the value of output op on chain, from the history in s.
func coinValue(s *pegState, chain string, op wire.OutPoint) (int64, bool) {
	t, ok := s.view(chain).funding[op.Hash]
	if !ok || int(op.Index) >= len(t.tx.TxOut) {
		return 0, false
	}
	return t.tx.TxOut[op.Index].Value, true
}

// spendsRecorded reports whether any of inputs is in recorded.
func spendsRecorded(inputs []utxo, recorded map[wire.OutPoint]bool) bool {
	for _, u := range inputs {
		if recorded[u.outPoint] {
			return true
		}
	}
	return false
}

// recordedSet is coins as a set.
func recordedSet(coins []string) map[wire.OutPoint]bool {
	set := map[wire.OutPoint]bool{}
	for _, raw := range coins {
		if op, err := parseOutPoint(raw); err == nil {
			set[op] = true
		}
	}
	return set
}

// fenceInputs is what a new fence on chain spends: the lineage's tip
// first (when the lineage has one), then every confirmed coin of the set
// there that nothing spends, oldest first, at most maxFenceInputs. The
// inputs come from the chain, not from recorded, so two signers whose
// cutovers began at different times build the same fence (Round 6, V5).
// recorded (this signer's coins) decides only whether a fence is needed:
// it fails if the fence would spend none of them, or while a transaction
// of the set other than a fence is pending there (a fence would conflict
// with it).
func (s *pegState) fenceInputs(chain string, recorded map[wire.OutPoint]bool) ([]utxo, error) {
	var anchor *utxo
	var free []utxo
	switch chain {
	case chainDogecoin:
		for _, t := range s.unconfirmed {
			if _, fence := parseFence(t.tx); !fence {
				return nil, errors.New("a Dogecoin transaction of the peg is pending; fence once it is in a block")
			}
		}
		anchor = s.pegAnchor
		for _, u := range s.lockedUTXOs {
			if u.confirmations > 0 {
				free = append(free, u)
			}
		}
	case chainDogecoinVM:
		for _, t := range s.vmUnconfirmed {
			if _, fence := parseFence(t.tx); !fence {
				return nil, errors.New("a DogecoinVM transaction of the reserve is pending; fence once it is in a block")
			}
		}
		anchor = s.reserveAnchor
		free = s.reserveUTXOs
	default:
		return nil, fmt.Errorf("unknown chain %q", chain)
	}
	if anchor != nil && !containsOutPoint(free, anchor.outPoint) {
		anchor = nil // spent in the mempool: by a fence, which a replacement re-spends
	}
	var rest []utxo
	for _, u := range free {
		if anchor == nil || u.outPoint != anchor.outPoint {
			rest = append(rest, u)
		}
	}
	sort.Slice(rest, func(i, j int) bool {
		if rest[i].confirmations != rest[j].confirmations {
			return rest[i].confirmations > rest[j].confirmations
		}
		return outPointLess(rest[i].outPoint, rest[j].outPoint)
	})
	var inputs []utxo
	if anchor != nil {
		inputs = append(inputs, *anchor)
	}
	inputs = append(inputs, rest...)
	if n := maxFenceInputs(chain); len(inputs) > n {
		inputs = inputs[:n] // the rest in the next fence
	}
	for _, u := range inputs {
		if recorded[u.outPoint] {
			return inputs, nil
		}
	}
	return nil, fmt.Errorf("no recorded coin on %s is left for a fence to spend", chain)
}

// fenceBase is the fee a fence fee is a multiple of on chain.
func (b *bridge) fenceBase(chain string) int64 {
	if chain == chainDogecoin {
		return b.dogeFee
	}
	return b.vmFee
}

// buildFence is the unsigned fence spending inputs (in order) back to the
// set's script, less fee, tagged with its first input. On Dogecoin it
// signals replaceability (BIP125), so a stuck one can be replaced. Signers
// rebuild it from their own view, so it depends only on its arguments.
func (b *bridge) buildFence(chain string, inputs []utxo, fee int64) (*wire.MsgTx, error) {
	if len(inputs) == 0 {
		return nil, errors.New("nothing to fence")
	}
	version := int32(wire.TxVersion)
	if chain == chainDogecoin {
		version = 1 // Dogecoin Core 1.14 relays version 1 and 2
	}
	var total int64
	tx := wire.NewMsgTx(version)
	for _, u := range inputs {
		in := wire.NewTxIn(&u.outPoint, nil, nil)
		if chain == chainDogecoin {
			in.Sequence = wire.MaxTxInSequenceNum - 2
		}
		tx.AddTxIn(in)
		total += u.value
	}
	if total-fee < pegDust {
		return nil, fmt.Errorf("the %s coins hold %s DOGE: not enough for the fence's %s DOGE fee and the next anchor",
			chain, formatDoge(total), formatDoge(fee))
	}
	tx.AddTxOut(wire.NewTxOut(total-fee, b.signers.pkScript()))
	tx.AddTxOut(nullData(encodeFence(inputs[0].outPoint)))
	return tx, nil
}

// fenceRedeems is the redeem script of each input of a fence.
func (b *bridge) fenceRedeems(s *pegState, chain string, inputs []utxo) ([][]byte, []destination, error) {
	redeems := make([][]byte, len(inputs))
	var register []destination
	for i, u := range inputs {
		if chain == chainDogecoinVM {
			redeems[i] = b.signers.redeemScript
			continue
		}
		if redeems[i] = s.redeemFor[string(u.pkScript)]; redeems[i] == nil {
			return nil, nil, fmt.Errorf("no redeem script for peg output %v", u.outPoint)
		}
		if d, ok := s.depositDest[string(u.pkScript)]; ok {
			register = append(register, d)
		}
	}
	return redeems, register, nil
}

// nextFence is the fence a cutover recording recorded calls for on chain
// now, as s reads it: a replacement of a stuck one (Dogecoin, after
// bumpAfter), or a new one. It returns the transaction, its inputs and its
// fee.
func (b *bridge) nextFence(s *pegState, chain string, recorded map[wire.OutPoint]bool, bumpAfter time.Duration) (*wire.MsgTx, []utxo, int64, error) {
	pending, shallow := fenceState(s, chain, recorded)
	base := b.fenceBase(chain)
	if pending != nil {
		if chain != chainDogecoin {
			return nil, nil, 0, fmt.Errorf("a fence on %s is waiting for a block; wait for it", chain)
		}
		if bumpAfter <= 0 {
			return nil, nil, 0, errors.New("a fence on dogecoin is unconfirmed, and replacements are off (-fence-bump-after 0)")
		}
		if waited := time.Since(time.Unix(pending.time, 0)); waited < bumpAfter {
			return nil, nil, 0, fmt.Errorf("a fence on dogecoin has waited %v of the %v before it may be replaced", waited.Round(time.Second), bumpAfter)
		}
		var inputs []utxo
		for _, in := range pending.tx.TxIn {
			c, ok := s.pendingCoins[in.PreviousOutPoint]
			if !ok {
				return nil, nil, 0, fmt.Errorf("the pending fence spends %v, which isn't a confirmed peg coin in this view", in.PreviousOutPoint)
			}
			inputs = append(inputs, c)
		}
		// One more fence fee each time: what bounds the series is the
		// signer's half-surplus share (feeRoom), checked on every one.
		size, err := b.fenceSizeOf(s, chain, inputs)
		if err != nil {
			return nil, nil, 0, err
		}
		fee := fenceFeeOf(s, chain, pending.tx) + fenceFee(chain, base, size)
		tx, err := b.buildFence(chain, inputs, fee)
		return tx, inputs, fee, err
	}
	if shallow {
		return nil, nil, 0, fmt.Errorf("a fence on %s is in a block and not yet deep enough: wait for it, rather than pay for another", chain)
	}
	inputs, err := s.fenceInputs(chain, recorded)
	if err != nil {
		return nil, nil, 0, err
	}
	if inputs, err = b.trimToSize(s, chain, inputs); err != nil {
		return nil, nil, 0, err
	}
	if !spendsRecorded(inputs, recorded) {
		return nil, nil, 0, fmt.Errorf("the next %s fence (as large as relay allows) spends none of this signer's recorded coins; a later one will", chain)
	}
	size, err := b.fenceSizeOf(s, chain, inputs)
	if err != nil {
		return nil, nil, 0, err
	}
	fee := fenceFee(chain, base, size)
	tx, err := b.buildFence(chain, inputs, fee)
	return tx, inputs, fee, err
}

// fenceSizeOf is the signed size of a fence spending inputs on chain.
func (b *bridge) fenceSizeOf(s *pegState, chain string, inputs []utxo) (int64, error) {
	redeems, _, err := b.fenceRedeems(s, chain, inputs)
	if err != nil {
		return 0, err
	}
	return fenceSize(redeems, b.signers.Required), nil
}

// signersCutover asks the remote signers for the coins their cutovers
// still record on chain and returns their union. Fence inputs come from
// the chain, so signers whose records differ (one began before a deposit
// arrived, one after) still build the same fence; the union only says
// whether any of them still needs one (Round 6, V5).
func (b *bridge) signersCutover(chain string) (map[wire.OutPoint]bool, error) {
	union := map[wire.OutPoint]bool{}
	reporting := 0
	var failures []string
	for _, r := range b.cosigners {
		var st struct {
			Cutover map[string][]string `json:"cutover"`
		}
		if err := r.status(&st); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.URL, err))
			continue
		}
		coins, in := st.Cutover[chain]
		if !in {
			continue
		}
		reporting++
		for op := range recordedSet(coins) {
			union[op] = true
		}
	}
	if reporting == 0 {
		if len(failures) > 0 {
			return nil, fmt.Errorf("no signer reports a cutover on %s (%s)", chain, strings.Join(failures, "; "))
		}
		return nil, fmt.Errorf("no signer is in a cutover on %s: no fence is needed there", chain)
	}
	return union, nil
}

// proposeFence proposes and sends the next fence on chain (or the
// replacement of a stuck one), and returns its txid.
func (b *bridge) proposeFence(chain string) (chainhash.Hash, error) {
	if chain != chainDogecoin && chain != chainDogecoinVM {
		return chainhash.Hash{}, fmt.Errorf("-fence takes %s or %s", chainDogecoin, chainDogecoinVM)
	}
	recorded, err := b.signersCutover(chain)
	if err != nil {
		return chainhash.Hash{}, err
	}
	return b.sendFence(chain, recorded)
}

// sendFence proposes and sends the fence a cutover recording recorded
// calls for on chain now.
func (b *bridge) sendFence(chain string, recorded map[wire.OutPoint]bool) (chainhash.Hash, error) {
	s, err := b.load()
	if err != nil {
		return chainhash.Hash{}, err
	}
	tx, inputs, _, err := b.nextFence(s, chain, recorded, b.fenceBumpAfter)
	if err != nil {
		return chainhash.Hash{}, err
	}
	redeems, register, err := b.fenceRedeems(s, chain, inputs)
	if err != nil {
		return chainhash.Hash{}, err
	}
	p := &proposal{Chain: chain, tx: tx, redeems: redeems, Register: register,
		Action: action{Kind: actionFence, Deposit: inputs[0].outPoint.String()}}
	if err := b.authorize(p); err != nil {
		return chainhash.Hash{}, err
	}
	if chain == chainDogecoin {
		return b.doge.send(tx)
	}
	return b.vm.send(tx)
}

// checkFence is a signer's check of a fence proposal on chain: its log is
// in a cutover there, and the proposal is the fence its own view calls for
// (nextFence). It returns that transaction, its redeem scripts, and its
// fee.
func (c *cosigner) checkFence(s *pegState, chain string) (*wire.MsgTx, [][]byte, int64, error) {
	coins, in := c.log.Cutover[chain]
	if !in {
		return nil, nil, 0, fmt.Errorf("this signer is in no cutover on %s: it signs no fence there", chain)
	}
	want, inputs, fee, err := c.b.nextFence(s, chain, recordedSet(coins), c.b.fenceBumpAfter)
	if err != nil {
		return nil, nil, 0, err
	}
	redeems, _, err := c.b.fenceRedeems(s, chain, inputs)
	if err != nil {
		return nil, nil, 0, err
	}
	return want, redeems, fee, nil
}
