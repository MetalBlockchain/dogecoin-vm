package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

func TestPause(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	dir := t.TempDir()
	setPath := filepath.Join(dir, "signers.json")
	require.NoError(writeSetFile(setPath, h.b.signers))
	h.b.signers.path = setPath

	alice := h.user(1)
	h.deposit(100*doge, &alice, 6)
	held := h.deposit(100*doge, nil, 6)

	// Paused: nothing is credited or refunded.
	require.NoError(cmdPause([]string{"-signers", setPath, "-reason", "investigating an alert."}))
	_, err := h.b.step()
	require.ErrorIs(err, errPaused)
	require.ErrorContains(err, "investigating an alert.")
	_, err = h.b.refund(wire.OutPoint{Hash: held.TxHash()}, h.user(9), false)
	require.ErrorIs(err, errPaused)
	require.Zero(paidTo(h.vm, alice))

	pause := (&healthChecker{b: h.b}).pauseCheck()
	require.False(pause.OK)
	require.Contains(pause.Detail, "investigating an alert.")

	// Resumed: the deposit made while paused is credited.
	require.NoError(cmdResume([]string{"-signers", setPath}))
	require.NotEmpty(h.step())
	require.Equal(int64(100*doge-doge/100), paidTo(h.vm, alice))

	// A signer paused by its own operator refuses to sign; the other two
	// still make 2 of 3.
	signerDir := t.TempDir()
	signerSet := filepath.Join(signerDir, setFileName)
	require.NoError(writeSetFile(signerSet, h.signers[0].b.signers))
	h.signers[0].b.signers.path = signerSet
	require.NoError(cmdPause([]string{"-dir", signerDir, "-reason", "operator away."}))
	h.vm.mine()
	h.pegOut(60*doge, h.user(2))
	require.NotEmpty(h.step())
	require.Equal(int64(59*doge), paidTo(h.doge, h.user(2)))

	// With a second signer paused, nothing moves.
	other := t.TempDir()
	require.NoError(writeSetFile(filepath.Join(other, setFileName), h.signers[1].b.signers))
	h.signers[1].b.signers.path = filepath.Join(other, setFileName)
	require.NoError(cmdPause([]string{"-dir", other, "-reason", "also away."}))
	h.doge.mine()
	h.pegOut(10*doge, h.user(3))
	_, err = h.b.step()
	require.ErrorContains(err, "this signer is paused")

	require.ErrorContains(cmdPause([]string{"-signers", setPath}), "-reason is required")
}
