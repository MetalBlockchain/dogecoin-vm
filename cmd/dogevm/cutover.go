package main

// The cutover fence. A signer whose log was rebuilt (adopt), or whose
// anchors were set rather than reached by its own signatures (signer-log
// anchors -set), knows nothing of signatures it may have made before: the
// live DogecoinVM signers signed for months without logs. Any of those the
// coordinator kept back could still be sent, and spend coins its anchor
// line knows nothing about. Matching anchors don't stop that; spending
// every coin those signatures could spend does.
//
// So on each chain such a log signs nothing but a fence first: one
// transaction spending the line's tip first and then every other confirmed
// coin the set holds there (on Dogecoin the peg address and every personal
// deposit address; on DogecoinVM the reserve), all of it back to the set's
// own script less a fee, its one output the line's next coin. A signer
// signs it only if it spends exactly the coins the signer's own view holds:
// a coin the coordinator left out could carry a kept-back signature. Once a
// fence is in a block deep enough (rootDepth on Dogecoin, so no
// reorganisation in the threat model undoes it), and no coin from an
// older block is left unspent, every signature made before it is dead: its
// inputs are spent. Only then (cutoverDone) does the signer sign anything
// else there.
//
// Deposits are unaffected: what is owed is read from the deposits'
// transactions, not from their coins still being where they were paid (as
// with a held deposit a payout pooled). The fee comes from the surplus.
// docs/RUNBOOK.md, "Cutover fence".

import (
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

const actionFence = "fence" // spend every coin of a chain once, after a log was rebuilt

var tagFence = []byte("DVMZ")

func encodeFence(anchor wire.OutPoint) []byte { return encodeOutPointTag(tagFence, anchor) }

// parseFence reads the anchor a fence names (its first input).
func parseFence(tx *wire.MsgTx) (wire.OutPoint, bool) { return parseOutPointTag(tx, tagFence) }

// maxFenceInputs keeps a fence a standard transaction (100 kB); a set
// holding more coins fences in several rounds (cutoverDone waits for all).
const maxFenceInputs = 250

// fenceFee is a fence's fee: base (the chain's per-transaction fee) for
// each started 10 kB of its estimated size, at least once. Both chains
// relay at 0.001 DOGE per kB, so it pays at least ten times that.
func fenceFee(base int64, inputs int) int64 {
	size := int64(10 + 300*inputs + 80)
	return base * max(1, (size+9_999)/10_000)
}

// fenceInputs is what a fence on chain spends, as s reads it: the line's
// tip, then every other confirmed coin the set holds there, by outpoint,
// at most maxFenceInputs. It fails while a transaction of the set is
// pending there: a fence would conflict with it.
func (s *pegState) fenceInputs(chain string) ([]utxo, error) {
	var anchor *utxo
	var coins []utxo
	switch chain {
	case chainDogecoin:
		if len(s.unconfirmed) > 0 {
			return nil, errors.New("a Dogecoin transaction of the peg is pending; fence once it is in a block")
		}
		anchor = s.pegAnchor
		for _, u := range s.lockedUTXOs {
			if u.confirmations > 0 {
				coins = append(coins, u)
			}
		}
	case chainDogecoinVM:
		if s.vmPending {
			return nil, errors.New("a DogecoinVM transaction of the reserve is pending; fence once it is in a block")
		}
		anchor = s.reserveAnchor
		coins = s.reserveUTXOs
	default:
		return nil, fmt.Errorf("unknown chain %q", chain)
	}
	if anchor == nil || !containsOutPoint(coins, anchor.outPoint) {
		return nil, fmt.Errorf("no free %s anchor to start a fence from", chain)
	}
	var rest []utxo
	for _, u := range coins {
		if u.outPoint != anchor.outPoint {
			rest = append(rest, u)
		}
	}
	sort.Slice(rest, func(i, j int) bool { return outPointLess(rest[i].outPoint, rest[j].outPoint) })
	inputs := append([]utxo{*anchor}, rest...)
	if len(inputs) > maxFenceInputs {
		inputs = inputs[:maxFenceInputs]
	}
	return inputs, nil
}

// buildFence is the unsigned fence spending inputs (the anchor first) back
// to the set's script, less fee, tagged with the anchor. Signers rebuild it
// from their own view, so it depends only on its arguments.
func (b *bridge) buildFence(chain string, inputs []utxo) (*wire.MsgTx, int64, error) {
	if len(inputs) == 0 {
		return nil, 0, errors.New("a fence spends at least the anchor")
	}
	base, version := b.vmFee, int32(wire.TxVersion)
	if chain == chainDogecoin {
		base, version = b.dogeFee, 1 // Dogecoin Core 1.14 relays version 1 and 2
	}
	fee := fenceFee(base, len(inputs))
	var total int64
	tx := wire.NewMsgTx(version)
	for _, u := range inputs {
		tx.AddTxIn(wire.NewTxIn(&u.outPoint, nil, nil))
		total += u.value
	}
	if total-fee < pegDust {
		return nil, 0, fmt.Errorf("the %s coins hold %s DOGE: not enough for the fence's %s DOGE fee and the next anchor",
			chain, formatDoge(total), formatDoge(fee))
	}
	tx.AddTxOut(wire.NewTxOut(total-fee, b.signers.pkScript()))
	tx.AddTxOut(nullData(encodeFence(inputs[0].outPoint)))
	return tx, fee, nil
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

// fence proposes and sends a fence on chain.
func (b *bridge) fence(chain string) (chainhash.Hash, error) {
	s, err := b.load()
	if err != nil {
		return chainhash.Hash{}, err
	}
	inputs, err := s.fenceInputs(chain)
	if err != nil {
		return chainhash.Hash{}, err
	}
	tx, _, err := b.buildFence(chain, inputs)
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

// checkFence is a signer's check of a fence proposal on chain: its log
// still needs one there, and tx spends the tip and every other coin its own
// view holds, as buildFence would. It returns the transaction it would
// build, its redeem scripts, and its fee.
func (c *cosigner) checkFence(s *pegState, chain string) (*wire.MsgTx, [][]byte, int64, error) {
	if !c.log.Unfenced[chain] {
		return nil, nil, 0, fmt.Errorf("this signer's log needs no fence on %s", chain)
	}
	inputs, err := s.fenceInputs(chain)
	if err != nil {
		return nil, nil, 0, err
	}
	want, fee, err := c.b.buildFence(chain, inputs)
	if err != nil {
		return nil, nil, 0, err
	}
	redeems, _, err := c.b.fenceRedeems(s, chain, inputs)
	if err != nil {
		return nil, nil, 0, err
	}
	return want, redeems, fee, nil
}

// cutoverDone reports whether a fence on chain has done its work for a log
// rebuilt with anchor from as its anchor there, as s reads it: a fence (a
// confirmed transaction tagged as one that spends coins of the set, so the
// signers made it), at least depth deep, starting from from or a later
// coin on the line (so it was signed after the log was rebuilt, not an
// older fence), with no coin the set held there in an older block left
// unspent. Every signature made before the log was rebuilt spent coins
// from older blocks, all of them now spent in blocks, so none can confirm.
func cutoverDone(s *pegState, chain string, from wire.OutPoint) (bool, string) {
	txs, depth, line := s.dogeTxs, int64(1), s.pegLine
	isCoin := func(out *wire.TxOut) bool { return s.redeemFor[string(out.PkScript)] != nil }
	if chain == chainDogecoinVM {
		txs, line = s.vmTxs, s.reserveLine
		script := s.reserveLine.script
		isCoin = func(out *wire.TxOut) bool { return string(out.PkScript) == string(script) }
	} else if s.pegLine != nil {
		depth = s.pegLine.rootDepth
	}
	coins := map[wire.OutPoint]bool{}
	for _, t := range txs {
		hash := t.tx.TxHash()
		for i, out := range t.tx.TxOut {
			if isCoin(out) {
				coins[wire.OutPoint{Hash: hash, Index: uint32(i)}] = true
			}
		}
	}
	// after reports whether op is from, or a later coin on its line.
	after := func(op wire.OutPoint) bool {
		cur := from
		for range len(coins) + 2 {
			if cur == op {
				return true
			}
			next, more, err := line.next(cur)
			if err != nil || !more {
				return false
			}
			cur = next
		}
		return false
	}
	spent := confirmedSpends(txs)
	why := fmt.Sprintf("no fence on %s from this log's anchor with %d confirmations yet", chain, depth)
fences:
	for _, f := range txs {
		anchor, ok := parseFence(f.tx)
		if !ok || f.confirmations < depth || len(f.tx.TxIn) == 0 || f.tx.TxIn[0].PreviousOutPoint != anchor ||
			!coins[anchor] || !after(anchor) {
			continue
		}
		for _, t := range txs {
			if t.confirmations <= f.confirmations {
				continue
			}
			hash := t.tx.TxHash()
			for i, out := range t.tx.TxOut {
				op := wire.OutPoint{Hash: hash, Index: uint32(i)}
				if isCoin(out) && spent[op] == nil {
					why = fmt.Sprintf("%v, from before the fence, is still unspent on %s: fence again", op, chain)
					continue fences
				}
			}
		}
		return true, ""
	}
	return false, why
}

// fenced reports whether this signer may sign anything but a fence on
// chain. A log that needed one stops needing it, once and for good, when
// the chain shows a fence from its anchor has done its work (cutoverDone).
func (c *cosigner) fenced(s *pegState, chain string) error {
	if !c.log.Unfenced[chain] {
		return nil
	}
	from, err := parseOutPoint(c.log.Anchors[chain])
	if err != nil {
		return fmt.Errorf("this signer's log needs a fence on %s but has no anchor there: %w", chain, err)
	}
	done, why := cutoverDone(s, chain, from)
	if !done {
		return fmt.Errorf("this signer's log was rebuilt or its anchors set: it signs only a fence on %s until one has spent every older coin there (%s; RUNBOOK.md, \"Cutover fence\")", chain, why)
	}
	delete(c.log.Unfenced, chain)
	return c.log.write()
}

// cmdFence is the operator's cutover step: dogevm fence -chain CHAIN.
func cmdFence(args []string) error {
	var s settings
	fs := flag.NewFlagSet("fence", flag.ExitOnError)
	signersPath := fs.String("signers", "", "peg signer set file")
	depositsPath := fs.String("deposits", "", "deposit address registry (default: deposits.json next to -signers)")
	chain := fs.String("chain", "", "dogecoin or dogecoinvm")
	s.register(fs)
	b := bridgeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := s.resolve(); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath, "chain": *chain}); err != nil {
		return err
	}
	if *chain != chainDogecoin && *chain != chainDogecoinVM {
		return fmt.Errorf("-chain must be %s or %s", chainDogecoin, chainDogecoinVM)
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	if err := b.connect(&s, signers); err != nil {
		return err
	}
	b.registry = registryFor(*depositsPath, *signersPath)
	b.contributionsPath = filepath.Join(filepath.Dir(*signersPath), "contributions.txt")
	txid, err := b.fence(*chain)
	if err != nil {
		return err
	}
	printJSON(map[string]string{"chain": *chain, "fenceTxid": txid.String()})
	return nil
}
