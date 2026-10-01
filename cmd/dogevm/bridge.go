package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"sort"
	"time"

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
	// coordinatorTLSCert and coordinatorTLSKey are the coordinator's
	// transport key pair, for signers over TLS (transport.go).
	coordinatorTLSCert, coordinatorTLSKey string
	flags                                 *flag.FlagSet    // the policy flags, if from bridgeFlags
	registry                              *depositRegistry // personal deposit addresses; may be nil
	vm, doge                              chain
	vmParams                              *chaincfg.Params
	dogeParams                            *chaincfg.Params

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
	// confFloor is the fewest Dogecoin confirmations any deposit is
	// credited at (minMainnetConfirmations on mainnet), whatever the tiers
	// say.
	confFloor int64
	// maxShallow caps credit from deposits short of full confirmations
	// (shallowCap); 0: the largest tier's amount.
	maxShallow int64
	// resent is when the bridge last handed each unconfirmed Dogecoin
	// transaction back to its node (rebroadcast).
	resent map[chainhash.Hash]time.Time

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
	redeemFor      map[string][]byte      // Dogecoin peg output script -> redeem script
	depositDest    map[string]destination // personal deposit script -> its destination
	reserveCreated int64                  // reserve paid in by consensus (coinbases)
	reserveUnspent int64                  // reserve still held
	reserveUTXOs   []utxo
	vmPending      bool // a release is still in the VM mempool
	// reserveAnchor and pegAnchor are the oldest confirmed coins of the
	// reserve and of the peg address. Every release spends reserveAnchor,
	// and every payout and refund spends pegAnchor, so any two transactions
	// the signers sign on one chain conflict and at most one confirms (see
	// oldestCoin).
	reserveAnchor *utxo
	pegAnchor     *utxo
	released      map[wire.OutPoint]chainhash.Hash // deposit -> release txid
	// cancelled maps a deposit cancelled on DogecoinVM (before a forced
	// refund) to whether that cancellation is in a block.
	cancelled map[wire.OutPoint]bool
	pegOuts   []pegOut
	deposits  []deposit
	paid      map[chainhash.Hash]chainhash.Hash // peg-out -> payment txid
	refunded  map[wire.OutPoint]chainhash.Hash  // deposit -> refund txid
	// vmUnconfirmed are the signers' DogecoinVM transactions (releases,
	// cancellations) not yet in a block, by txid.
	vmUnconfirmed map[chainhash.Hash]chainTx
	// unconfirmed are Dogecoin payments and refunds not yet in a block.
	unconfirmed     map[chainhash.Hash]chainTx
	held            []deposit // deposits not credited: no destination, or outside the limits
	settled         []deposit // deposits refunded instead of credited
	locked          int64     // DOGE held at the peg address on Dogecoin
	lockedUTXOs     []utxo
	unclaimedOnDoge int64 // deposits not credited: owed back as refunds
	unclaimedOnVM   int64 // untagged or too-small payments into the reserve: owed back
	contributed     int64 // DVMC payments to the peg address: operating capital
	// pendingRefunds is deposits refunded by a transaction not yet in a
	// block: still owed until it is (its inputs still count as locked).
	pendingRefunds int64
	// pendingFees is the surplus already promised to cancellations signed
	// but not yet in a block (a signer's unseenFees).
	pendingFees int64
	// vmTxs and dogeTxs are the transactions read from each chain, for a
	// signer's check of its log against them (fence.go).
	vmTxs, dogeTxs []chainTx
}

// audit is the peg's solvency check.
type audit struct {
	Circulating     int64 `json:"circulating"`     // DOGE released onto DogecoinVM
	PendingPegIns   int64 `json:"pendingPegIns"`   // deposits not yet credited
	PendingPegOuts  int64 `json:"pendingPegOuts"`  // peg-outs not yet paid
	Locked          int64 `json:"locked"`          // DOGE held on Dogecoin
	Required        int64 `json:"required"`        // circulating + pending
	Surplus         int64 `json:"surplus"`         // locked - required: contributions (operating capital)
	UnclaimedOnDoge int64 `json:"unclaimedOnDoge"` // part of required: deposits owed back as refunds
	UnclaimedOnVM   int64 `json:"unclaimedOnVM"`   // part of required: reserve payments owed back
	Contributed     int64 `json:"contributed"`     // DVMC operating capital paid in
	PendingRefunds  int64 `json:"pendingRefunds"`  // part of required: refunds sent, not yet in a block
	PendingFees     int64 `json:"pendingFees"`     // part of required: fees of cancellations signed, not yet in a block
}

// solvent: what is locked covers what is owed. Negative circulation can't
// happen unless the reserve is miscounted, so it fails too.
func (a audit) solvent() bool { return a.Surplus >= 0 && a.Circulating >= 0 }

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
		vmUnconfirmed: map[chainhash.Hash]chainTx{},
		cancelled:     map[wire.OutPoint]bool{},
		released:      map[wire.OutPoint]chainhash.Hash{},
		paid:          map[chainhash.Hash]chainhash.Hash{},
		refunded:      map[wire.OutPoint]chainhash.Hash{},

		unconfirmed: map[chainhash.Hash]chainTx{},
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
	s.vmTxs = vmTxs
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
			if deposit, ok := parseCancel(t.tx); ok {
				s.cancelled[deposit] = s.cancelled[deposit] || t.confirmations > 0
			}
			if t.confirmations == 0 {
				s.vmPending = true
				s.vmUnconfirmed[t.tx.TxHash()] = t
			}
		default:
			if t.confirmations == 0 {
				continue // not final yet
			}
			dest, ok := parseDestinationTag(t.tx, tagPegOut)
			// A payout to the peg address itself would look like change on
			// Dogecoin, so it is never made.
			ok = ok && !bytes.Equal(dest.pkScript(), script)
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
	// DogecoinVM's node still lists a confirmed output that a transaction
	// in its mempool spends, so the outputs known transactions spend are
	// left out here: otherwise a release in flight counts both its input
	// and its change, and the reserve looks larger than it is.
	spentByKnown := map[wire.OutPoint]bool{}
	for _, t := range vmTxs {
		for _, in := range t.tx.TxIn {
			spentByKnown[in.PreviousOutPoint] = true
		}
	}
	// The anchor comes from what is in blocks alone, so a release still in
	// the mempool doesn't move it.
	var reserveCoins []utxo
	for _, u := range held {
		if u.confirmations > 0 {
			reserveCoins = append(reserveCoins, u)
		}
	}
	s.reserveAnchor = anchorOf(reserveCoins, script, signerSpends, 1)
	for _, u := range held {
		if spentByKnown[u.outPoint] {
			continue
		}
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
	s.dogeTxs = dogeTxs
	pegOuts := map[wire.OutPoint]bool{}
	for _, t := range dogeTxs {
		hash := t.tx.TxHash()
		for i, out := range t.tx.TxOut {
			if s.redeemFor[string(out.PkScript)] != nil {
				pegOuts[wire.OutPoint{Hash: hash, Index: uint32(i)}] = true
			}
		}
	}
	// Transactions the signers made: only they can spend the peg's coins.
	made := map[chainhash.Hash]bool{}
	for _, t := range dogeTxs {
		if spendsAny(t.tx, pegOuts) {
			made[t.tx.TxHash()] = true
		}
	}
	var all []deposit
	for _, t := range dogeTxs {
		if spendsAny(t.tx, pegOuts) {
			// Only the signers can spend peg outputs; outputs back to the
			// peg are change, not deposits.
			hash := t.tx.TxHash()
			if t.confirmations == 0 {
				s.unconfirmed[hash] = t
			}
			// If an action has two transactions listed, the one in a block
			// is the one that counts.
			counts := func(prev chainhash.Hash, listed bool) bool {
				_, prevPending := s.unconfirmed[prev]
				return !listed || (prevPending && t.confirmations > 0)
			}
			if request, ok := parsePayment(t.tx); ok {
				if prev, listed := s.paid[request]; counts(prev, listed) {
					s.paid[request] = hash
				}
			}
			if deposit, ok := parseRefund(t.tx); ok {
				if prev, listed := s.refunded[deposit]; counts(prev, listed) {
					s.refunded[deposit] = hash
				}
			}
			// A payout to a personal deposit address is a deposit to it.
			for i, out := range t.tx.TxOut {
				if dest, personal := depositDest[string(out.PkScript)]; personal {
					all = append(all, deposit{time: t.time, outPoint: wire.OutPoint{Hash: hash, Index: uint32(i)},
						value: out.Value, dest: dest, valid: b.depositInRange(out.Value), confirmations: t.confirmations})
				}
			}
			continue
		}
		tagDest, hasTag := parseDestinationTag(t.tx, tagDeposit)
		contribution := isContribution(t.tx)
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
			case bytes.Equal(out.PkScript, script) && contribution:
				// Operating capital: part of what is locked, owed to no one.
				if t.confirmations > 0 {
					s.contributed += out.Value
				}
				continue
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
			// Owed until the refund is in a block: until then it can be
			// evicted, and its inputs still count as locked.
			if _, pending := s.unconfirmed[s.refunded[d.outPoint]]; pending && d.confirmations > 0 {
				s.pendingRefunds += d.value
			}
		case d.valid && !isCancelled(s, d.outPoint):
			s.deposits = append(s.deposits, d)
		case s.released[d.outPoint] != (chainhash.Hash{}):
			// Credited while it was in range (a cap changed since): it
			// backs what circulates, and is owed back to no one.
			continue
		default:
			s.held = append(s.held, d)
			if d.confirmations > 0 {
				s.unclaimedOnDoge += d.value
			}
		}
	}
	s.lockedUTXOs, err = b.doge.unspent(dogeAddrs, 0)
	if err != nil {
		return nil, fmt.Errorf("reading Dogecoin peg addresses: %w", err)
	}
	// The audit reads only what's in blocks, so nothing in the mempool (a
	// deposit that may be double-spent, a payout that is dropped) moves it.
	// Locked DOGE is the peg's confirmed outputs, including those the
	// signers' unconfirmed payments spend, which the wallet no longer lists
	// as unspent; such a payment's peg-out stays owed until it confirms.
	for _, u := range s.lockedUTXOs {
		if u.confirmations > 0 {
			s.locked += u.value
		}
	}
	confirmedCoin := map[wire.OutPoint]utxo{}
	for _, t := range dogeTxs {
		if t.confirmations > 0 {
			hash := t.tx.TxHash()
			for i, out := range t.tx.TxOut {
				if s.redeemFor[string(out.PkScript)] != nil {
					op := wire.OutPoint{Hash: hash, Index: uint32(i)}
					confirmedCoin[op] = utxo{outPoint: op, value: out.Value, pkScript: out.PkScript, confirmations: t.confirmations}
				}
			}
		}
	}
	pendingSpent := map[wire.OutPoint]bool{}
	for _, t := range s.unconfirmed {
		for _, in := range t.tx.TxIn {
			if c, ok := confirmedCoin[in.PreviousOutPoint]; ok && !pendingSpent[in.PreviousOutPoint] {
				pendingSpent[in.PreviousOutPoint] = true
				s.locked += c.value
			}
		}
	}
	// The anchor is chosen from what is in blocks alone: the wallet stops
	// listing an output a mempool transaction spends, so those are added
	// back, and a transaction someone shows this node without mining it
	// can't move the anchor.
	var pegCoins []utxo
	for _, u := range s.lockedUTXOs {
		if u.confirmations > 0 {
			pegCoins = append(pegCoins, u)
		}
	}
	for op := range pendingSpent {
		pegCoins = append(pegCoins, confirmedCoin[op])
	}
	s.pegAnchor = anchorOf(pegCoins, script, made, max(b.depositConfirmations, b.confFloor))

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
		Contributed:     s.contributed,
		PendingRefunds:  s.pendingRefunds,
		PendingFees:     s.pendingFees,
	}
	for _, d := range s.deposits {
		if _, done := s.released[d.outPoint]; !done && d.confirmations > 0 {
			a.PendingPegIns += d.value
		}
	}
	for _, p := range s.pegOuts {
		payment, paid := s.paid[p.txid]
		if _, pending := s.unconfirmed[payment]; !paid || pending {
			a.PendingPegOuts += p.value
		}
	}
	// Deposits not credited and payments into the reserve that weren't
	// valid peg-outs are owed back to whoever paid them, so they are never
	// surplus.
	a.Required = a.Circulating + a.PendingPegIns + a.PendingPegOuts + a.UnclaimedOnDoge + a.UnclaimedOnVM + a.PendingRefunds + a.PendingFees
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
	b.rebroadcast(s)

	var failed error
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
			if err := b.shallowRoom(s, d, 0); err != nil {
				continue // credited once deeper
			}
			txid, err := b.release(s, d)
			if err != nil {
				// Go on to the next: one that can't be credited mustn't
				// hold up the rest.
				failed = errors.Join(failed, fmt.Errorf("releasing deposit %v: %w", d.outPoint, err))
				continue
			}
			b.logWaiting(failed)
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
			failed = errors.Join(failed, fmt.Errorf("paying peg-out %v: %w", p.txid, err))
			continue
		}
		b.logWaiting(failed)
		return fmt.Sprintf("paid %s DOGE for peg-out %v in %v",
			formatDoge(p.value-b.dogeFee), p.txid, txid), nil
	}
	return "", failed
}

// logWaiting logs what couldn't be done in a step that did something else.
func (b *bridge) logWaiting(failed error) {
	if failed != nil {
		b.logf("waiting: %v", failed)
	}
}

// refundable checks a deposit has the confirmations a credit of it would
// need: a refund of one that is not settled in a block could be paid while
// its sender double-spends it.
func (b *bridge) refundable(d deposit) error {
	if need := b.confirmationsFor(d.value); d.confirmations < need {
		return fmt.Errorf("deposit %v has %d of %d confirmations; it can be refunded once it has them", d.outPoint, d.confirmations, need)
	}
	return nil
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
	inputs, total, err := s.releaseInputs(d.value)
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
	inputs, err := s.payoutInputs(confirmed, value)
	if err != nil {
		return chainhash.Hash{}, err
	}
	tx, err := b.buildPayout(inputs, value, dest, data)
	if err != nil {
		return chainhash.Hash{}, err
	}
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
// Dogecoin fee, to dest from inputs, the first of them the peg's anchor.
// Like buildRelease, signers rebuild it, so it depends only on its
// arguments.
//
// Every payout pays something back to the shared peg address, which every
// signer watches from the start: that output is the next anchor. Change
// below pegDust is topped up from the payout.
func (b *bridge) buildPayout(inputs []utxo, value int64, dest destination, data []byte) (*wire.MsgTx, error) {
	var total int64
	tx := wire.NewMsgTx(1) // Dogecoin Core 1.14 relays version 1 and 2
	for _, u := range inputs {
		tx.AddTxIn(wire.NewTxIn(&u.outPoint, nil, nil))
		total += u.value
	}
	if total < value {
		return nil, fmt.Errorf("inputs hold %s DOGE, need %s", formatDoge(total), formatDoge(value))
	}
	// Every input but the anchor must be needed: an extra one would only
	// move peg coins around.
	for i := 1; i < len(inputs); i++ {
		if total-inputs[i].value >= value {
			return nil, fmt.Errorf("spends %v, which the payout does not need", inputs[i].outPoint)
		}
	}
	tx.AddTxOut(wire.NewTxOut(0, dest.pkScript())) // value set below
	change, topUp := total-value, int64(0)
	if change < pegDust {
		change, topUp = pegDust, pegDust-change
	}
	tx.AddTxOut(wire.NewTxOut(change, b.signers.pkScript()))
	tx.AddTxOut(nullData(data))
	pays := value - b.dogeFee - topUp
	if pays < pegDust {
		return nil, fmt.Errorf("%s DOGE doesn't cover the %s DOGE fee and the peg's change", formatDoge(value), formatDoge(b.dogeFee))
	}
	tx.TxOut[0].Value = pays
	return tx, nil
}

// pegDust is the least the peg's change output holds: Dogecoin Core's
// dust limit (DEFAULT_DUST_LIMIT, 0.01 DOGE since 1.14.5), so the output
// relays without an extra fee.
const pegDust = koinuPerDoge / 100

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
	if done, ok := s.cancelled[op]; ok && !done {
		return chainhash.Hash{}, errors.New("the deposit's cancellation on DogecoinVM isn't in a block yet; run this again in a moment")
	}
	for _, d := range s.held {
		if d.outPoint == op {
			if err := b.refundable(d); err != nil {
				return chainhash.Hash{}, err
			}
			return b.payFromPeg(s, d.value, dest, encodeRefund(op), refundAction(op, dest))
		}
	}
	for _, d := range s.deposits {
		if d.outPoint == op {
			if !force {
				return chainhash.Hash{}, errCreditable
			}
			// Still creditable: cancel it on DogecoinVM first. A refund then
			// follows on the next run, once the cancellation is in a block.
			txid, err := b.cancel(s, op)
			if err != nil {
				return chainhash.Hash{}, err
			}
			return txid, errCancelSent
		}
	}
	return chainhash.Hash{}, errNotRefundable
}

// errCancelSent: a forced refund's first step is done; run it again.
var errCancelSent = errors.New("cancelled the deposit on DogecoinVM; run the refund again once that is in a block")

// isCancelled reports whether deposit op was cancelled on DogecoinVM (in a
// block or not): it is never credited, only refunded.
func isCancelled(s *pegState, op wire.OutPoint) bool {
	_, ok := s.cancelled[op]
	return ok
}

// buildCancel is the unsigned DogecoinVM transaction cancelling deposit op:
// it spends the reserve's anchor alone, back to the reserve less the fee,
// tagged with op. Any release of op signed and kept back spends that same
// anchor (or an earlier one, already spent), so once this is in a block
// (final on DogecoinVM) no release of op can ever confirm.
func (b *bridge) buildCancel(anchor utxo, op wire.OutPoint) (*wire.MsgTx, error) {
	if anchor.value <= b.vmFee {
		return nil, errors.New("the reserve's anchor is too small to pay a cancellation's fee")
	}
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(&anchor.outPoint, nil, nil))
	tx.AddTxOut(wire.NewTxOut(anchor.value-b.vmFee, b.signers.pkScript()))
	tx.AddTxOut(nullData(encodeCancel(op)))
	return tx, nil
}

// cancel proposes and sends the DogecoinVM cancellation of deposit op.
func (b *bridge) cancel(s *pegState, op wire.OutPoint) (chainhash.Hash, error) {
	if s.vmPending {
		return chainhash.Hash{}, errors.New("a DogecoinVM transaction of the peg is pending; try again in a moment")
	}
	if s.reserveAnchor == nil {
		return chainhash.Hash{}, errors.New("the reserve has no anchor")
	}
	tx, err := b.buildCancel(*s.reserveAnchor, op)
	if err != nil {
		return chainhash.Hash{}, err
	}
	p := &proposal{Chain: chainDogecoinVM, tx: tx, redeems: [][]byte{b.signers.redeemScript},
		Action: action{Kind: actionCancel, Deposit: op.String()}}
	if err := b.authorize(p); err != nil {
		return chainhash.Hash{}, err
	}
	return b.vm.send(tx)
}

// dogecoinHardDust is Dogecoin Core's DEFAULT_HARD_DUST_LIMIT.
const dogecoinHardDust = koinuPerDoge / 1000

// oldestCoin returns the coin in the oldest block, ties broken by outpoint,
// or nil. It is the anchor every signed transaction on a chain must spend
// first.
//
// Why: each signer's log stops it signing two transactions for one action
// that could both confirm, but that is per signer. With 2 of 3, a dishonest
// coordinator and one dishonest signer B could have A and B sign a payout
// from some coins and B and C sign the same payout again from other coins;
// neither honest signer signs twice, and both payouts confirm. If every
// transaction must spend the oldest coin, any two of them conflict. The
// anchor can't change until a transaction spending it is in a block: on
// Dogecoin it is chosen only from coins the signers made at the peg address
// (anchorOf), which only ever appear in block order.
func oldestCoin(coins []utxo) *utxo {
	var best *utxo
	for i := range coins {
		u := &coins[i]
		if best == nil || u.confirmations > best.confirmations ||
			u.confirmations == best.confirmations && outPointLess(u.outPoint, best.outPoint) {
			best = u
		}
	}
	if best == nil {
		return nil
	}
	c := *best
	return &c
}

// anchorOf is the anchor among the peg's confirmed coins on one chain: the
// coin every transaction the signers sign there spends first, so any two
// of them conflict and at most one confirms.
//
// The anchor follows the signers' own transactions. Once they have made
// one at script (the peg address on Dogecoin, the reserve on DogecoinVM),
// only coins they made there (outputs of transactions in made, which spend
// the peg's coins and so could only be theirs) can anchor; each spends the
// anchor before it and pays change back to script, so the anchor moves one
// step at a time along that chain (signingLog.anchorMoved follows the same
// steps). Coins others pay to script (deposits tagged to the peg address,
// contributions, peg-outs into the reserve) never anchor, however old:
// otherwise the anchor could jump from the chain's tip to one of them while
// a transaction spending the tip was signed and kept back. Personal deposit
// addresses aren't script at all: one can be registered, and an old payment
// to it imported, at any time.
//
// Before the signers have made anything at script, the anchor is its
// oldest coin with at least rootDepth confirmations, deep enough that no
// reorganisation takes it away.
func anchorOf(coins []utxo, script []byte, made map[chainhash.Hash]bool, rootDepth int64) *utxo {
	var theirs, root []utxo
	for _, u := range coins {
		if !bytes.Equal(u.pkScript, script) {
			continue
		}
		if made[u.outPoint.Hash] {
			theirs = append(theirs, u)
		} else if u.confirmations >= rootDepth {
			root = append(root, u)
		}
	}
	if len(theirs) > 0 {
		return oldestCoin(theirs)
	}
	return oldestCoin(root)
}

func outPointLess(a, b wire.OutPoint) bool {
	if c := bytes.Compare(a.Hash[:], b.Hash[:]); c != 0 {
		return c < 0
	}
	return a.Index < b.Index
}

// anchoredSelection is the inputs a signed transaction spends: the anchor
// first, then, if it doesn't cover need, what pick chooses from the rest.
func anchoredSelection(anchor utxo, available []utxo, need int64) ([]utxo, int64, error) {
	if anchor.value >= need {
		return []utxo{anchor}, anchor.value, nil
	}
	var rest []utxo
	for _, u := range available {
		if u.outPoint != anchor.outPoint {
			rest = append(rest, u)
		}
	}
	more, total, err := selectUTXOs(rest, need-anchor.value)
	if err != nil {
		return nil, 0, err
	}
	return append([]utxo{anchor}, more...), anchor.value + total, nil
}

func containsOutPoint(coins []utxo, op wire.OutPoint) bool {
	for _, u := range coins {
		if u.outPoint == op {
			return true
		}
	}
	return false
}

// payoutInputs is what a payout or refund of value spends: the peg's
// anchor, and if it doesn't cover the payout, more confirmed coins. While a
// transaction in the mempool spends the anchor, nothing else can be paid: it
// would conflict.
func (s *pegState) payoutInputs(confirmed []utxo, value int64) ([]utxo, error) {
	if s.pegAnchor == nil {
		return nil, errors.New("the peg address holds no confirmed DOGE to anchor a payout (contribute to it once, docs/RUNBOOK.md, or wait for a payout's change to confirm)")
	}
	if !containsOutPoint(confirmed, s.pegAnchor.outPoint) {
		return nil, fmt.Errorf("waiting for the payment spending %v to confirm", s.pegAnchor.outPoint)
	}
	inputs, _, err := anchoredSelection(*s.pegAnchor, confirmed, value)
	return inputs, err
}

// releaseInputs is what a release of value spends: the reserve's anchor,
// and more of the reserve if it doesn't cover the release.
func (s *pegState) releaseInputs(value int64) ([]utxo, int64, error) {
	if s.reserveAnchor == nil || !containsOutPoint(s.reserveUTXOs, s.reserveAnchor.outPoint) {
		return nil, 0, errors.New("waiting for the reserve's oldest coin to be free")
	}
	return anchoredSelection(*s.reserveAnchor, s.reserveUTXOs, value)
}

// resendEvery is how often an unconfirmed payment or refund is handed back
// to the Dogecoin node.
const resendEvery = 10 * time.Minute

// resender is a chain that can hand a transaction back to its node,
// accepting that it may already be there.
type resender interface {
	resend(tx *wire.MsgTx) error
}

// rebroadcast hands every unconfirmed Dogecoin transaction of the peg back
// to the node every resendEvery. A node can drop one (a restart, a full
// mempool), and Dogecoin has no fee replacement to send it again; without
// this, such a payment would sit listed as paid and never confirm, holding
// up every later payout behind the anchor it spends.
func (b *bridge) rebroadcast(s *pegState) {
	if b.resent == nil {
		b.resent = map[chainhash.Hash]time.Time{}
	}
	now := time.Now()
	for _, side := range []struct {
		c   chain
		txs map[chainhash.Hash]chainTx
	}{{b.doge, s.unconfirmed}, {b.vm, s.vmUnconfirmed}} {
		r, ok := side.c.(resender)
		if !ok {
			continue
		}
		for txid, t := range side.txs {
			if now.Sub(b.resent[txid]) < resendEvery {
				continue
			}
			b.resent[txid] = now
			if err := r.resend(t.tx); err != nil {
				b.logf("rebroadcasting %v: %v", txid, err)
			}
		}
	}
	for txid := range b.resent {
		_, doge := s.unconfirmed[txid]
		_, vm := s.vmUnconfirmed[txid]
		if !doge && !vm {
			delete(b.resent, txid)
		}
	}
}

func isCoinbase(tx *wire.MsgTx) bool {
	return len(tx.TxIn) == 1 && tx.TxIn[0].PreviousOutPoint.Index == wire.MaxPrevOutIndex &&
		tx.TxIn[0].PreviousOutPoint.Hash == chainhash.Hash{}
}
