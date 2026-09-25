package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"sort"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
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
	signers *signerSet
	// cosigners are remote signers, each holding one key, asked to sign
	// what this process cannot sign with the keys in signers.
	cosigners          []*remoteSigner
	cosignersPath      string
	coordinatorKeyPath string
	flags              *flag.FlagSet    // the policy flags, if from bridgeFlags
	registry           *depositRegistry // personal deposit addresses; may be nil
	vm, doge           chain
	vmParams           *chaincfg.Params
	dogeParams         *chaincfg.Params

	depositConfirmations int64
	confirmationTiers    []confirmationTier // smaller deposits need fewer; see tiers.go
	tiersFlag            string
	vmFee                int64 // deducted from each credit to pay the VM fee
	dogeFee              int64 // deducted from each peg-out to pay the Dogecoin fee
	minDeposit           int64
	minPegOut            int64
	// maxDeposit and maxCirculating cap what the bridge will credit, so a
	// bug or a compromised signer can only lose so much. Deposits above
	// maxDeposit are never credited; a deposit that would take circulating
	// DOGE past maxCirculating waits. Either stays locked on Dogecoin for a
	// manual refund. Zero means no cap.
	maxDeposit     int64
	maxCirculating int64

	logf func(format string, args ...any)
}

type deposit struct {
	time          int64
	outPoint      wire.OutPoint
	value         int64
	dest          destination
	valid         bool // has a destination and meets the minimum
	confirmations int64
}

type pegOut struct {
	time          int64
	txid          chainhash.Hash
	value         int64
	dest          destination
	valid         bool
	confirmations int64
}

// pegState is everything the bridge knows, read from both chains.
type pegState struct {
	redeemFor       map[string][]byte      // Dogecoin peg output script -> redeem script
	depositDest     map[string]destination // personal deposit script -> its destination
	reserveCreated  int64                  // reserve paid in by consensus (coinbases)
	reserveUnspent  int64                  // reserve still held
	reserveUTXOs    []utxo
	vmPending       bool                             // a release is still in the VM mempool
	released        map[wire.OutPoint]chainhash.Hash // deposit -> release txid
	pegOuts         []pegOut
	deposits        []deposit
	paid            map[chainhash.Hash]chainhash.Hash // peg-out -> payment txid
	refunded        map[wire.OutPoint]chainhash.Hash  // deposit -> refund txid
	held            []deposit                         // deposits not credited: no destination, or outside the limits
	settled         []deposit                         // deposits refunded instead of credited
	locked          int64                             // DOGE held at the peg address on Dogecoin
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

// dogeWatchSet returns the Dogecoin addresses holding the peg: the peg
// address and each registered personal deposit address. It fills
// s.redeemFor and returns the destination of each deposit script.
func (b *bridge) dogeWatchSet(s *pegState) ([]btcutil.Address, map[string]destination, error) {
	pegAddr, err := b.dogePegAddress()
	if err != nil {
		return nil, nil, err
	}
	addrs := []btcutil.Address{pegAddr}
	s.redeemFor = map[string][]byte{string(b.signers.pkScript()): b.signers.redeemScript}
	depositDest := map[string]destination{}

	dests, err := b.registry.list()
	if err != nil {
		return nil, nil, err
	}
	for _, d := range dests {
		redeem := b.signers.depositRedeemScript(d)
		addr, err := btcutil.NewAddressScriptHash(redeem, b.dogeParams)
		if err != nil {
			return nil, nil, err
		}
		addrs = append(addrs, addr)
		s.redeemFor[string(p2shScript(redeem))] = redeem
		depositDest[string(p2shScript(redeem))] = d
	}
	return addrs, depositDest, nil
}

// depositInRange reports whether a deposit of value is credited at all.
func (b *bridge) depositInRange(value int64) bool {
	return value >= b.minDeposit && (b.maxDeposit == 0 || value <= b.maxDeposit)
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
		released: map[wire.OutPoint]chainhash.Hash{},
		paid:     map[chainhash.Hash]chainhash.Hash{},
		refunded: map[wire.OutPoint]chainhash.Hash{},
	}

	// DogecoinVM side.
	reserveAddr, err := b.vmReserveAddress()
	if err != nil {
		return nil, err
	}
	vmTxs, err := b.vm.txsFor([]btcutil.Address{reserveAddr})
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
				s.released[deposit] = t.tx.TxHash()
			}
			if t.confirmations == 0 {
				s.vmPending = true
			}
		default:
			if t.confirmations == 0 {
				continue // not final yet
			}
			dest, ok := parseDestinationTag(t.tx, tagPegOut)
			p := pegOut{time: t.time, txid: t.tx.TxHash(), value: paidIn, dest: dest,
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
	held, err := b.vm.unspent([]btcutil.Address{reserveAddr}, 0)
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

	// Dogecoin side: the peg address and every personal deposit address.
	dogeAddrs, depositDest, err := b.dogeWatchSet(s)
	if err != nil {
		return nil, err
	}
	s.depositDest = depositDest
	dogeTxs, err := b.doge.txsFor(dogeAddrs)
	if err != nil {
		return nil, fmt.Errorf("reading Dogecoin peg addresses: %w", err)
	}
	pegOuts := map[wire.OutPoint]bool{}
	for _, t := range dogeTxs {
		hash := t.tx.TxHash()
		for i, out := range t.tx.TxOut {
			if s.redeemFor[string(out.PkScript)] != nil {
				pegOuts[wire.OutPoint{Hash: hash, Index: uint32(i)}] = true
			}
		}
	}
	var all []deposit
	for _, t := range dogeTxs {
		if spendsAny(t.tx, pegOuts) {
			// Only the signers can spend peg outputs; outputs back to the
			// peg are change, not deposits.
			if request, ok := parsePayment(t.tx); ok {
				s.paid[request] = t.tx.TxHash()
			}
			if deposit, ok := parseRefund(t.tx); ok {
				s.refunded[deposit] = t.tx.TxHash()
			}
			continue
		}
		tagDest, hasTag := parseDestinationTag(t.tx, tagDeposit)
		hash := t.tx.TxHash()
		for i, out := range t.tx.TxOut {
			d := deposit{
				time:          t.time,
				outPoint:      wire.OutPoint{Hash: hash, Index: uint32(i)},
				value:         out.Value,
				confirmations: t.confirmations,
			}
			switch dest, personal := depositDest[string(out.PkScript)]; {
			case personal:
				// A personal deposit address names its destination.
				d.dest, d.valid = dest, b.depositInRange(out.Value)
			case bytes.Equal(out.PkScript, script):
				// The shared peg address needs a DVMD tag.
				d.dest, d.valid = tagDest, hasTag && b.depositInRange(out.Value)
			default:
				continue
			}
			all = append(all, d)
		}
	}
	// A refunded deposit is settled: never credited, no longer owed.
	for _, d := range all {
		switch {
		case s.refunded[d.outPoint] != (chainhash.Hash{}):
			s.settled = append(s.settled, d)
		case d.valid:
			s.deposits = append(s.deposits, d)
		default:
			s.held = append(s.held, d)
			s.unclaimedOnDoge += d.value
		}
	}
	s.lockedUTXOs, err = b.doge.unspent(dogeAddrs, 0)
	if err != nil {
		return nil, fmt.Errorf("reading Dogecoin peg addresses: %w", err)
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
		if _, done := s.released[d.outPoint]; !done {
			a.PendingPegIns += d.value
		}
	}
	for _, p := range s.pegOuts {
		if _, done := s.paid[p.txid]; !done {
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
	if p := b.paused(); p != nil {
		return "", p.err()
	}
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
			if _, done := s.released[d.outPoint]; done || d.confirmations < b.confirmationsFor(d.value) {
				continue
			}
			if b.maxCirculating > 0 && s.reserveCreated-s.reserveUnspent+d.value > b.maxCirculating {
				b.logf("holding deposit %v: crediting %s DOGE would exceed the %s DOGE cap",
					d.outPoint, formatDoge(d.value), formatDoge(b.maxCirculating))
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
		if _, done := s.paid[p.txid]; done {
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
	tx := b.buildRelease(inputs, total, d)
	redeems := make([][]byte, len(tx.TxIn))
	for i := range redeems {
		redeems[i] = b.signers.redeemScript
	}
	p := &proposal{
		Chain: chainDogecoinVM, tx: tx, redeems: redeems,
		Action:   action{Kind: actionRelease, Deposit: d.outPoint.String()},
		Register: []destination{d.dest},
	}
	if err := b.authorize(p); err != nil {
		return chainhash.Hash{}, err
	}
	return b.vm.send(tx)
}

// buildRelease is the unsigned transaction crediting d from inputs, which
// hold total. Signers rebuild it to check a proposal, so it must depend
// only on its arguments.
func (b *bridge) buildRelease(inputs []utxo, total int64, d deposit) *wire.MsgTx {
	tx := wire.NewMsgTx(wire.TxVersion)
	for _, u := range inputs {
		tx.AddTxIn(wire.NewTxIn(&u.outPoint, nil, nil))
	}
	tx.AddTxOut(wire.NewTxOut(d.value-b.vmFee, d.dest.pkScript()))
	if change := total - d.value; change > 0 {
		tx.AddTxOut(wire.NewTxOut(change, b.signers.pkScript()))
	}
	tx.AddTxOut(nullData(encodeRelease(d.outPoint)))
	return tx
}

// pay pays peg-out p on Dogecoin from the peg address. The peg gives up the
// full peg-out; the Dogecoin fee comes out of the payment.
func (b *bridge) pay(s *pegState, p pegOut) (chainhash.Hash, error) {
	return b.payFromPeg(s, p.value, p.dest, encodePayment(p.txid),
		action{Kind: actionPayout, PegOut: p.txid.String()})
}

// payFromPeg pays value, less the Dogecoin fee, to dest from confirmed peg
// outputs on Dogecoin, tagged with data. Change returns to the peg address.
func (b *bridge) payFromPeg(s *pegState, value int64, dest destination, data []byte, why action) (chainhash.Hash, error) {
	var confirmed []utxo
	for _, u := range s.lockedUTXOs {
		if u.confirmations > 0 {
			confirmed = append(confirmed, u)
		}
	}
	if value <= b.dogeFee {
		return chainhash.Hash{}, fmt.Errorf("%s DOGE does not cover the %s DOGE fee", formatDoge(value), formatDoge(b.dogeFee))
	}
	inputs, total, err := selectUTXOs(confirmed, value)
	if err != nil {
		return chainhash.Hash{}, err
	}
	tx := b.buildPayout(inputs, total, value, dest, data)
	redeems := make([][]byte, len(inputs))
	var register []destination
	for i, u := range inputs {
		if redeems[i] = s.redeemFor[string(u.pkScript)]; redeems[i] == nil {
			return chainhash.Hash{}, fmt.Errorf("no redeem script for peg output %v", u.outPoint)
		}
		if d, ok := s.depositDest[string(u.pkScript)]; ok {
			register = append(register, d)
		}
	}
	p := &proposal{Chain: chainDogecoin, Action: why, tx: tx, redeems: redeems, Register: register}
	if err := b.authorize(p); err != nil {
		return chainhash.Hash{}, err
	}
	return b.doge.send(tx)
}

// buildPayout is the unsigned Dogecoin transaction paying value, less the
// Dogecoin fee, to dest from inputs, which hold total. Like buildRelease,
// signers rebuild it, so it depends only on its arguments.
func (b *bridge) buildPayout(inputs []utxo, total, value int64, dest destination, data []byte) *wire.MsgTx {
	tx := wire.NewMsgTx(1) // Dogecoin Core 1.14 relays version 1 and 2
	for _, u := range inputs {
		tx.AddTxIn(wire.NewTxIn(&u.outPoint, nil, nil))
	}
	tx.AddTxOut(wire.NewTxOut(value-b.dogeFee, dest.pkScript()))
	// Change below Dogecoin's hard dust limit cannot be relayed, so it
	// goes to the fee.
	if change := total - value; change >= dogecoinHardDust {
		tx.AddTxOut(wire.NewTxOut(change, b.signers.pkScript()))
	}
	tx.AddTxOut(nullData(data))
	return tx
}

var (
	errNotRefundable = errors.New("not a deposit the bridge holds")
	errCreditable    = errors.New("the bridge may still credit this deposit; stop the bridge and pass -force to refund it")
)

// refund returns deposit op, less the Dogecoin fee, to dest. Deposits the
// bridge will never credit (no destination, outside the limits) can always
// be refunded. One it may still credit (it is waiting for confirmations or
// for room under maxCirculating) needs force, and the bridge must be
// stopped first so the two cannot race.
func (b *bridge) refund(op wire.OutPoint, dest destination, force bool) (chainhash.Hash, error) {
	if p := b.paused(); p != nil {
		return chainhash.Hash{}, p.err()
	}
	s, err := b.load()
	if err != nil {
		return chainhash.Hash{}, err
	}
	if txid, done := s.refunded[op]; done {
		return chainhash.Hash{}, fmt.Errorf("already refunded in %v", txid)
	}
	if txid, done := s.released[op]; done {
		return chainhash.Hash{}, fmt.Errorf("already credited in %v", txid)
	}
	for _, d := range s.held {
		if d.outPoint == op {
			return b.payFromPeg(s, d.value, dest, encodeRefund(op), refundAction(op, dest))
		}
	}
	for _, d := range s.deposits {
		if d.outPoint == op {
			if !force {
				return chainhash.Hash{}, errCreditable
			}
			return b.payFromPeg(s, d.value, dest, encodeRefund(op), refundAction(op, dest))
		}
	}
	return chainhash.Hash{}, errNotRefundable
}

// dogecoinHardDust is Dogecoin Core's DEFAULT_HARD_DUST_LIMIT.
const dogecoinHardDust = koinuPerDoge / 1000

func isCoinbase(tx *wire.MsgTx) bool {
	return len(tx.TxIn) == 1 && tx.TxIn[0].PreviousOutPoint.Index == wire.MaxPrevOutIndex &&
		tx.TxIn[0].PreviousOutPoint.Hash == chainhash.Hash{}
}
