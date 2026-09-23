package main

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// bridge moves DOGE between Dogecoin and DogecoinVM.
//
// Peg-in: a Dogecoin deposit to the peg address carrying a DVMD tag is, once
// it has depositConfirmations, credited on DogecoinVM from the peg reserve
// by a release tagged DVMI. Peg-out: a DogecoinVM payment to the reserve
// carrying a DVMO tag is paid out on Dogecoin from the peg address by a
// payment tagged DVMR. DogecoinVM transactions are final once in a block.
//
// All state is read back from the two chains, so a restarted bridge picks up
// where it left off, and nothing is ever credited or paid twice. Release and
// payment tags are only trusted on transactions that spend peg outputs,
// which only the signers can create.
type bridge struct {
	signers    *signerSet
	vm, doge   chain
	vmParams   *chaincfg.Params
	dogeParams *chaincfg.Params

	depositConfirmations int64
	vmFee                int64 // deducted from each credit to pay the VM fee
	dogeFee              int64 // deducted from each peg-out to pay the Dogecoin fee
	minDeposit           int64
	minPegOut            int64

	logf func(format string, args ...any)
}

type deposit struct {
	outPoint      wire.OutPoint
	value         int64
	dest          destination
	valid         bool // has a destination and meets the minimum
	confirmations int64
}

type pegOut struct {
	txid          chainhash.Hash
	value         int64
	dest          destination
	valid         bool
	confirmations int64
}

// pegState is everything the bridge knows, read from both chains.
type pegState struct {
	reserveCreated  int64 // reserve paid in by consensus (coinbases)
	reserveUnspent  int64 // reserve still held
	reserveUTXOs    []utxo
	vmPending       bool // a release is still in the VM mempool
	released        map[wire.OutPoint]bool
	pegOuts         []pegOut
	deposits        []deposit
	paid            map[chainhash.Hash]bool
	locked          int64 // DOGE held at the peg address on Dogecoin
	lockedUTXOs     []utxo
	unclaimedOnDoge int64 // deposits without a usable destination
	unclaimedOnVM   int64 // untagged or too-small payments into the reserve
}

// audit is the peg's solvency check.
type audit struct {
	Circulating     int64 `json:"circulating"`     // DOGE released onto DogecoinVM
	PendingPegIns   int64 `json:"pendingPegIns"`   // deposits not yet credited
	PendingPegOuts  int64 `json:"pendingPegOuts"`  // peg-outs not yet paid
	Locked          int64 `json:"locked"`          // DOGE held on Dogecoin
	Required        int64 `json:"required"`        // circulating + pending
	Surplus         int64 `json:"surplus"`         // locked - required
	UnclaimedOnDoge int64 `json:"unclaimedOnDoge"` // part of surplus
	UnclaimedOnVM   int64 `json:"unclaimedOnVM"`
}

func (a audit) solvent() bool { return a.Surplus >= 0 }

func (b *bridge) vmReserveAddress() (btcutil.Address, error) {
	return b.signers.address(b.vmParams)
}

func (b *bridge) dogePegAddress() (btcutil.Address, error) {
	return b.signers.address(b.dogeParams)
}

// spendsAny reports whether tx spends one of outs.
func spendsAny(tx *wire.MsgTx, outs map[wire.OutPoint]bool) bool {
	for _, in := range tx.TxIn {
		if outs[in.PreviousOutPoint] {
			return true
		}
	}
	return false
}

// outputsTo returns the outpoints and total value tx pays to script.
func outputsTo(tx *wire.MsgTx, script []byte) (map[wire.OutPoint]bool, int64) {
	hash := tx.TxHash()
	outs := map[wire.OutPoint]bool{}
	var total int64
	for i, out := range tx.TxOut {
		if bytes.Equal(out.PkScript, script) {
			outs[wire.OutPoint{Hash: hash, Index: uint32(i)}] = true
			total += out.Value
		}
	}
	return outs, total
}

func (b *bridge) load() (*pegState, error) {
	script := b.signers.pkScript()
	s := &pegState{
		released: map[wire.OutPoint]bool{},
		paid:     map[chainhash.Hash]bool{},
	}

	// DogecoinVM side.
	reserveAddr, err := b.vmReserveAddress()
	if err != nil {
		return nil, err
	}
	vmTxs, err := b.vm.txsFor(reserveAddr)
	if err != nil {
		return nil, fmt.Errorf("reading DogecoinVM reserve: %w", err)
	}
	reserveOuts := map[wire.OutPoint]bool{}
	for _, t := range vmTxs {
		outs, _ := outputsTo(t.tx, script)
		for op := range outs {
			reserveOuts[op] = true
		}
	}
	for _, t := range vmTxs {
		_, paidIn := outputsTo(t.tx, script)
		switch {
		case isCoinbase(t.tx):
			if t.confirmations > 0 {
				s.reserveCreated += paidIn
			}
		case spendsAny(t.tx, reserveOuts):
			// Only the signers can spend the reserve.
			if deposit, ok := parseRelease(t.tx); ok {
				s.released[deposit] = true
			}
			if t.confirmations == 0 {
				s.vmPending = true
			}
		default:
			if t.confirmations == 0 {
				continue // not final yet
			}
			dest, ok := parseDestinationTag(t.tx, tagPegOut)
			p := pegOut{txid: t.tx.TxHash(), value: paidIn, dest: dest,
				valid: ok && paidIn >= b.minPegOut, confirmations: t.confirmations}
			if p.valid {
				s.pegOuts = append(s.pegOuts, p)
			} else {
				s.unclaimedOnVM += paidIn
			}
		}
	}
	// The reserve still held includes the change of releases waiting in the
	// mempool, whose reserve inputs already count as spent. Other
	// unconfirmed payments into the reserve are not final yet.
	signerSpends := map[chainhash.Hash]bool{}
	for _, t := range vmTxs {
		if !isCoinbase(t.tx) && spendsAny(t.tx, reserveOuts) {
			signerSpends[t.tx.TxHash()] = true
		}
	}
	held, err := b.vm.unspent(reserveAddr, 0)
	if err != nil {
		return nil, fmt.Errorf("reading DogecoinVM reserve: %w", err)
	}
	for _, u := range held {
		if u.confirmations == 0 && !signerSpends[u.outPoint.Hash] {
			continue
		}
		s.reserveUnspent += u.value
		if u.confirmations > 0 {
			s.reserveUTXOs = append(s.reserveUTXOs, u)
		}
	}

	// Dogecoin side.
	pegAddr, err := b.dogePegAddress()
	if err != nil {
		return nil, err
	}
	dogeTxs, err := b.doge.txsFor(pegAddr)
	if err != nil {
		return nil, fmt.Errorf("reading Dogecoin peg address: %w", err)
	}
	pegOuts := map[wire.OutPoint]bool{}
	for _, t := range dogeTxs {
		outs, _ := outputsTo(t.tx, script)
		for op := range outs {
			pegOuts[op] = true
		}
	}
	for _, t := range dogeTxs {
		if spendsAny(t.tx, pegOuts) {
			// Only the signers can spend from the peg; outputs back to it
			// are change, not deposits.
			if request, ok := parsePayment(t.tx); ok {
				s.paid[request] = true
			}
			continue
		}
		dest, hasDest := parseDestinationTag(t.tx, tagDeposit)
		hash := t.tx.TxHash()
		for i, out := range t.tx.TxOut {
			if !bytes.Equal(out.PkScript, script) {
				continue
			}
			d := deposit{
				outPoint:      wire.OutPoint{Hash: hash, Index: uint32(i)},
				value:         out.Value,
				dest:          dest,
				valid:         hasDest && out.Value >= b.minDeposit,
				confirmations: t.confirmations,
			}
			if d.valid {
				s.deposits = append(s.deposits, d)
			} else {
				s.unclaimedOnDoge += d.value
			}
		}
	}
	s.lockedUTXOs, err = b.doge.unspent(pegAddr, 0)
	if err != nil {
		return nil, fmt.Errorf("reading Dogecoin peg address: %w", err)
	}
	for _, u := range s.lockedUTXOs {
		s.locked += u.value
	}

	// Oldest first, so the bridge works through them in order.
	sort.Slice(s.deposits, func(i, j int) bool {
		return s.deposits[i].confirmations > s.deposits[j].confirmations
	})
	sort.Slice(s.pegOuts, func(i, j int) bool {
		return s.pegOuts[i].confirmations > s.pegOuts[j].confirmations
	})
	return s, nil
}

func (b *bridge) audit(s *pegState) audit {
	a := audit{
		Circulating:     s.reserveCreated - s.reserveUnspent,
		Locked:          s.locked,
		UnclaimedOnDoge: s.unclaimedOnDoge,
		UnclaimedOnVM:   s.unclaimedOnVM,
	}
	for _, d := range s.deposits {
		if !s.released[d.outPoint] {
			a.PendingPegIns += d.value
		}
	}
	for _, p := range s.pegOuts {
		if !s.paid[p.txid] {
			a.PendingPegOuts += p.value
		}
	}
	a.Required = a.Circulating + a.PendingPegIns + a.PendingPegOuts
	a.Surplus = a.Locked - a.Required
	return a
}

var errInsolvent = errors.New("peg is insolvent: DOGE locked on Dogecoin is less than what DogecoinVM owes; refusing to act")

// step performs at most one release or payment. It returns what it did, or
// "" if there was nothing to do.
func (b *bridge) step() (string, error) {
	s, err := b.load()
	if err != nil {
		return "", err
	}
	if a := b.audit(s); !a.solvent() {
		return "", fmt.Errorf("%w (%+v)", errInsolvent, a)
	}

	// Releases chain off each other's reserve change, so wait for the
	// previous one to be accepted.
	if !s.vmPending {
		for _, d := range s.deposits {
			if s.released[d.outPoint] || d.confirmations < b.depositConfirmations {
				continue
			}
			txid, err := b.release(s, d)
			if err != nil {
				return "", fmt.Errorf("releasing deposit %v: %w", d.outPoint, err)
			}
			return fmt.Sprintf("credited %s DOGE for deposit %v in %v",
				formatDoge(d.value-b.vmFee), d.outPoint, txid), nil
		}
	}

	for _, p := range s.pegOuts {
		if s.paid[p.txid] {
			continue
		}
		txid, err := b.pay(s, p)
		if err != nil {
			return "", fmt.Errorf("paying peg-out %v: %w", p.txid, err)
		}
		return fmt.Sprintf("paid %s DOGE for peg-out %v in %v",
			formatDoge(p.value-b.dogeFee), p.txid, txid), nil
	}
	return "", nil
}

// selectUTXOs picks outputs, largest first, until they cover amount.
func selectUTXOs(utxos []utxo, amount int64) ([]utxo, int64, error) {
	sorted := append([]utxo(nil), utxos...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].value > sorted[j].value })
	var picked []utxo
	var total int64
	for _, u := range sorted {
		if total >= amount {
			break
		}
		picked = append(picked, u)
		total += u.value
	}
	if total < amount {
		return nil, 0, fmt.Errorf("only %s DOGE available, need %s", formatDoge(total), formatDoge(amount))
	}
	return picked, total, nil
}

// release credits d on DogecoinVM from the reserve. The reserve gives up the
// full deposit; the VM fee comes out of the credit.
func (b *bridge) release(s *pegState, d deposit) (chainhash.Hash, error) {
	inputs, total, err := selectUTXOs(s.reserveUTXOs, d.value)
	if err != nil {
		return chainhash.Hash{}, err
	}
	tx := wire.NewMsgTx(wire.TxVersion)
	for _, u := range inputs {
		tx.AddTxIn(wire.NewTxIn(&u.outPoint, nil, nil))
	}
	tx.AddTxOut(wire.NewTxOut(d.value-b.vmFee, d.dest.pkScript()))
	if change := total - d.value; change > 0 {
		tx.AddTxOut(wire.NewTxOut(change, b.signers.pkScript()))
	}
	tx.AddTxOut(nullData(encodeRelease(d.outPoint)))
	if err := b.signers.sign(tx); err != nil {
		return chainhash.Hash{}, err
	}
	return b.vm.send(tx)
}

// pay pays peg-out p on Dogecoin from the peg address. The peg gives up the
// full peg-out; the Dogecoin fee comes out of the payment.
func (b *bridge) pay(s *pegState, p pegOut) (chainhash.Hash, error) {
	var confirmed []utxo
	for _, u := range s.lockedUTXOs {
		if u.confirmations > 0 {
			confirmed = append(confirmed, u)
		}
	}
	inputs, total, err := selectUTXOs(confirmed, p.value)
	if err != nil {
		return chainhash.Hash{}, err
	}
	tx := wire.NewMsgTx(1) // Dogecoin Core 1.14 relays version 1 and 2
	for _, u := range inputs {
		tx.AddTxIn(wire.NewTxIn(&u.outPoint, nil, nil))
	}
	tx.AddTxOut(wire.NewTxOut(p.value-b.dogeFee, p.dest.pkScript()))
	// Change below Dogecoin's hard dust limit cannot be relayed, so it
	// goes to the fee.
	if change := total - p.value; change >= dogecoinHardDust {
		tx.AddTxOut(wire.NewTxOut(change, b.signers.pkScript()))
	}
	tx.AddTxOut(nullData(encodePayment(p.txid)))
	if err := b.signers.sign(tx); err != nil {
		return chainhash.Hash{}, err
	}
	return b.doge.send(tx)
}

// dogecoinHardDust is Dogecoin Core's DEFAULT_HARD_DUST_LIMIT.
const dogecoinHardDust = koinuPerDoge / 1000

func isCoinbase(tx *wire.MsgTx) bool {
	return len(tx.TxIn) == 1 && tx.TxIn[0].PreviousOutPoint.Index == wire.MaxPrevOutIndex &&
		tx.TxIn[0].PreviousOutPoint.Hash == chainhash.Hash{}
}
