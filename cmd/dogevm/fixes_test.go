package main

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// These cover fixes first found in a review of the BTCVM port of this
// bridge, whose code this shares.

// A held deposit is refunded only once it has the confirmations a credit
// of it would need, so a refund can't be paid while its sender
// double-spends the deposit.
func TestUnconfirmedDepositIsNotRefunded(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice := h.user(1)
	h.deposit(50*doge, &alice, 6) // something to pay from
	require.NotEmpty(h.step())
	h.vm.mine()
	held := h.deposit(30*doge, nil, 0) // no destination: held
	op := wire.OutPoint{Hash: held.TxHash()}

	_, err := h.b.refund(op, h.user(9), false)
	require.ErrorContains(err, "0 of 6 confirmations")
	for i := 0; i < 6; i++ {
		h.doge.mine()
	}
	_, err = h.b.refund(op, h.user(9), false)
	require.NoError(err)
}

// After a policy change makes a credited deposit look held (here, a higher
// minimum), a signer still refuses to refund it.
func TestSignerNeverRefundsACreditedDeposit(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	alice := h.user(1)
	dep := h.deposit(3*doge, &alice, 6)
	h.deposit(50*doge, &alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()
	require.NotEmpty(h.step())
	h.vm.mine()

	op := wire.OutPoint{Hash: dep.TxHash()}
	c := h.signers[0]
	c.b.minDeposit = 5 * doge
	s, err := c.b.load()
	require.NoError(err)
	_, held := findDeposit(s.held, op)
	require.False(held, "a credited deposit is never held, nor owed back, even outside the limits")

	back := h.user(9)
	from, err := s.payoutInputs(func() []utxo {
		var confirmed []utxo
		for _, u := range s.lockedUTXOs {
			if u.confirmations > 0 {
				confirmed = append(confirmed, u)
			}
		}
		return confirmed
	}(), 3*doge)
	require.NoError(err)
	tx, err := h.b.buildPayout(from, 3*doge, back, encodeRefund(op), h.b.dogeFee)
	require.NoError(err)
	_, _, _, err = c.check(signRequest{Chain: chainDogecoin, Tx: encodeTx(tx), Action: refundAction(op, back)})
	require.ErrorContains(err, "was credited")
}

// A withdrawal paid to a personal deposit address is a deposit to it,
// credited again, not stranded.
func TestPayoutToADepositAddressIsCredited(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice := h.user(1)
	_, err := h.b.registry.add(alice)
	require.NoError(err)
	h.personalDeposit(100*doge, alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()

	depositAddr, err := h.b.signers.depositAddress(alice, h.b.dogeParams)
	require.NoError(err)
	back, err := destinationOf(depositAddr)
	require.NoError(err)
	h.pegOut(40*doge, back)
	require.NotEmpty(h.step())
	for i := 0; i < 6; i++ {
		h.doge.mine()
	}
	require.NotEmpty(h.step(), "the payout is credited as a deposit")
	h.vm.mine()
	a := h.audit()
	require.True(a.solvent(), "%+v", a)
	require.Zero(a.PendingPegIns)
	require.Equal(40*doge-h.b.dogeFee-h.b.vmFee, paidTo(h.vm, alice))
}

// A withdrawal naming the peg address itself would look like change, so it
// is never paid.
func TestPegOutToThePegAddressIsRefused(t *testing.T) {
	h := newHarness(t)
	alice := h.user(1)
	h.deposit(100*doge, &alice, 6)
	require.NotEmpty(t, h.step())
	h.vm.mine()
	h.pegOut(40*doge, h.b.signers.destination())
	require.Empty(t, h.step())
	require.Equal(t, int64(40*doge), h.audit().UnclaimedOnVM)
}

// A payout waits while the one before it, which spends the peg's anchor,
// is unconfirmed: they would conflict. Once it confirms, the rest are paid
// in turn, each from the next anchor.
func TestPayoutsWaitForTheAnchor(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	alice := h.user(1)
	h.deposit(100*doge, &alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()
	h.deposit(20*doge, &alice, 6)
	require.NotEmpty(h.step())
	h.vm.mine()

	h.pegOut(60*doge, h.user(2))
	require.Contains(h.step(), "paid")
	h.pegOut(25*doge, h.user(3))
	h.pegOut(5*doge, h.user(4))
	did, err := h.b.step()
	require.Empty(did)
	require.ErrorContains(err, "waiting for the payment spending", "the anchor is spent by a payout still in the mempool")
	h.doge.mine()

	require.Contains(h.step(), "paid")
	h.doge.mine()
	require.Contains(h.step(), "paid")
	h.doge.mine()
	a := h.audit()
	require.True(a.solvent(), "%+v", a)
	require.Zero(a.PendingPegOuts)
}
