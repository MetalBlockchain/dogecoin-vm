package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
	"github.com/stretchr/testify/require"
)

// dropTx takes tx out of the fake chain entirely: its history and its
// outputs, as a wallet that lost it sees.
func dropTx(c *fakeChain, hash chainhash.Hash) {
	kept := c.txs[:0]
	for _, t := range c.txs {
		if t.tx.TxHash() != hash {
			kept = append(kept, t)
		}
	}
	c.txs = kept
}

// TestMinedButMissingEndToEnd (Codex r12 #1 and #3): one coherent fixture.
// Deposit P is mined; a pooled spend F of P's output and the peg anchor is
// mined; then the wallet loses P from every read (history and UTXOs) while
// still listing F. Without the retained record the books would drop P's
// claim and overstate the surplus by its value. With it, the signer refuses
// to load or sign, and so do the public readers (the web server's health
// check and audit, which read the same file without writing it); nothing
// in the cutover moves; a restart changes nothing; importing P restores the
// exact books.
func TestMinedButMissingEndToEnd(t *testing.T) {
	require := require.New(t)
	h := newCosignHarness(t)
	signer := h.signers[0]
	s0, err := signer.b.load()
	require.NoError(err)
	before := signer.b.audit(s0)
	depth := s0.pegLine.rootDepth

	alice := h.user(1)
	p := h.deposit(5*doge, &alice, 30)
	pid := p.TxHash().String()
	core := &stubDogecoinCore{listed: map[string]int64{pid: 3}, bodies: map[string]string{pid: encodeTx(p)}}
	srv := core.serve(t)
	path := filepath.Join(t.TempDir(), deadHistoryFile)
	attach := func(b *bridge, readOnly bool) {
		b.doge = coreConflicts{fakeChain: h.doge, core: &dogeChain{rpc: newRPCClient(srv.URL, "", ""),
			dead: &deadHistory{path: path, depth: depth, readOnly: readOnly}}}
	}
	attach(signer.b, false)
	s1, err := signer.b.load() // the wallet lists P in a block: recorded as mined
	require.NoError(err)
	withP := signer.b.audit(s1)
	require.Equal(before.PendingPegIns+5*doge, withP.PendingPegIns, "P is owed")

	// A release of P, built while the wallet still lists it.
	d, ok := findDeposit(s1.deposits, wire.OutPoint{Hash: p.TxHash(), Index: 0})
	require.True(ok)
	inputs, total, err := s1.releaseInputs(d.value)
	require.NoError(err)
	release := signRequest{Chain: chainDogecoinVM, Tx: encodeTx(h.b.buildRelease(inputs, total, d)),
		Register: []string{encodeDest(alice)}, Action: action{Kind: actionRelease, Deposit: d.outPoint.String()}}

	// F pools P's coin with the peg anchor back into the peg.
	require.NotNil(s1.pegAnchor)
	const fee = doge / 10
	f := wire.NewMsgTx(1)
	f.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: p.TxHash(), Index: 0}, nil, nil))
	f.AddTxIn(wire.NewTxIn(&s1.pegAnchor.outPoint, nil, nil))
	f.AddTxOut(wire.NewTxOut(5*doge+s1.pegAnchor.value-fee, h.b.signers.pkScript()))
	fid := f.TxHash().String()
	h.doge.add(f, 3)
	core.listed[fid] = 3
	core.bodies[fid] = encodeTx(f)

	// The wallet loses P from every read.
	dropTx(h.doge, p.TxHash())
	delete(core.listed, pid)

	// Without the record (the old public readers) P's claim is gone and the
	// surplus is overstated by exactly P's value.
	naive, err := h.b.load()
	require.NoError(err)
	wrong := h.b.audit(naive)
	require.Equal(withP.Surplus-fee+5*doge, wrong.Surplus, "the hole the record closes")

	history, cutover, epoch := signer.log.History, len(signer.log.Cutover), signer.log.CutoverEpoch
	_, err = signer.b.load()
	require.ErrorIs(err, errUnresolvedHistory)
	require.ErrorContains(err, fid, "names the spender it saw")
	_, _, _, err = signer.check(release)
	require.ErrorIs(err, errUnresolvedHistory, "a signature is refused")
	require.Equal(history, signer.log.History, "the cutover doesn't move")
	require.Len(signer.log.Cutover, cutover)
	require.Equal(epoch, signer.log.CutoverEpoch)

	// The public readers, read-only on the same file, refuse too.
	file, err := os.ReadFile(path)
	require.NoError(err)
	web := *h.b
	attach(&web, true)
	_, err = web.load()
	require.ErrorIs(err, errUnresolvedHistory, "audit refuses")
	checks := (&healthChecker{b: &web}).pegChecks(nil, err)
	var historyCheck *check
	for i := range checks {
		if checks[i].Name == "history" {
			historyCheck = &checks[i]
		}
		if checks[i].Name == "peg" {
			require.False(checks[i].OK, "no peg reported healthy")
		}
	}
	require.NotNil(historyCheck, "health names the problem")
	require.False(historyCheck.OK)
	after, err := os.ReadFile(path)
	require.NoError(err)
	require.True(bytes.Equal(file, after), "a read-only reader writes nothing")

	// A restart changes nothing: the record is on disk.
	attach(signer.b, false)
	_, err = signer.b.load()
	require.True(errors.Is(err, errUnresolvedHistory), "still refused after a restart")

	// Imported: P is back in both reads, and the books are exact.
	h.doge.add(p, 33)
	core.listed[pid] = 3
	for _, restart := range []bool{false, true} {
		if restart {
			attach(signer.b, false)
		}
		s, err := signer.b.load()
		require.NoError(err)
		a := signer.b.audit(s)
		require.Equal(withP.PendingPegIns, a.PendingPegIns, "P is owed again")
		require.Equal(withP.Surplus-fee, a.Surplus, "the surplus fell by F's fee only")
		require.True(a.solvent())
		_, err = web.load()
		require.NoError(err, "and the public readers read again")
	}
}
