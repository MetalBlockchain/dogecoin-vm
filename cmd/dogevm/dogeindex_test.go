package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// fakeDoge is a Dogecoin chain for the index: blocks of transactions, a
// mempool, and nothing else.
type fakeDoge struct {
	blocks [][]*wire.MsgTx // blocks[h] are block h's transactions, coinbase first
	hashes []chainhash.Hash
	pool   []*wire.MsgTx
	fork   byte // changes block hashes, to simulate a reorganisation
	coins  uint32
}

func newFakeDoge() *fakeDoge {
	f := &fakeDoge{}
	f.mine() // genesis
	return f
}

func (f *fakeDoge) coinbase() *wire.MsgTx {
	f.coins++
	tx := wire.NewMsgTx(1)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(&chainhash.Hash{}, wire.MaxPrevOutIndex), []byte{byte(f.coins), byte(f.coins >> 8), f.fork}, nil))
	tx.AddTxOut(wire.NewTxOut(10_000*doge, []byte{0x51}))
	return tx
}

// mine makes a block of the mempool's transactions.
func (f *fakeDoge) mine() {
	txs := append([]*wire.MsgTx{f.coinbase()}, f.pool...)
	f.pool = nil
	f.blocks = append(f.blocks, txs)
	var h chainhash.Hash
	h[0], h[1], h[2] = byte(len(f.blocks)), byte(len(f.blocks)>>8), f.fork
	f.hashes = append(f.hashes, h)
}

// reorg replaces the last n blocks with new ones holding the same
// transactions (their coinbases differ) and, if drop, loses the final
// block's other transactions back to the mempool.
func (f *fakeDoge) reorg(n int, drop bool) {
	f.fork++
	kept := f.blocks[:len(f.blocks)-n]
	old := f.blocks[len(f.blocks)-n:]
	f.blocks, f.hashes = kept, f.hashes[:len(kept)]
	for i, b := range old {
		if drop && i == len(old)-1 {
			// The replacement block leaves them out: back to the mempool.
			f.pool = nil
			f.mine()
			f.pool = append(f.pool, b[1:]...)
			continue
		}
		f.pool = b[1:]
		f.mine()
	}
}

func (f *fakeDoge) pay(from *wire.OutPoint, value int64, script []byte) *wire.MsgTx {
	tx := wire.NewMsgTx(1)
	if from == nil {
		f.coins++
		from = &wire.OutPoint{Hash: chainhash.Hash{0xee, byte(f.coins)}}
	}
	tx.AddTxIn(wire.NewTxIn(from, []byte{1}, nil))
	tx.AddTxOut(wire.NewTxOut(value, script))
	f.pool = append(f.pool, tx)
	return tx
}

func (f *fakeDoge) synced() (bool, error)      { return true, nil }
func (f *fakeDoge) blockCount() (int64, error) { return int64(len(f.blocks) - 1), nil }
func (f *fakeDoge) blockHash(h int64) (chainhash.Hash, error) {
	if h < 0 || int(h) >= len(f.hashes) {
		return chainhash.Hash{}, errors.New("no such block")
	}
	return f.hashes[h], nil
}
func (f *fakeDoge) blockInfo(hash chainhash.Hash) (chainhash.Hash, int64, []chainhash.Hash, error) {
	for h, x := range f.hashes {
		if x == hash {
			var prev chainhash.Hash
			if h > 0 {
				prev = f.hashes[h-1]
			}
			var ids []chainhash.Hash
			for _, tx := range f.blocks[h] {
				ids = append(ids, tx.TxHash())
			}
			return prev, int64(1_700_000_000 + h*60), ids, nil
		}
	}
	return chainhash.Hash{}, 0, nil, errors.New("no such block")
}
func (f *fakeDoge) rawTx(txid chainhash.Hash) (*wire.MsgTx, error) {
	for _, b := range f.blocks {
		for _, tx := range b {
			if tx.TxHash() == txid {
				return tx, nil
			}
		}
	}
	for _, tx := range f.pool {
		if tx.TxHash() == txid {
			return tx, nil
		}
	}
	return nil, errors.New("no such transaction")
}
func (f *fakeDoge) txBlock(txid chainhash.Hash) (int64, int64, error) {
	for h, b := range f.blocks {
		for _, tx := range b {
			if tx.TxHash() == txid {
				return int64(h), int64(1_700_000_000 + h*60), nil
			}
		}
	}
	return 0, 0, nil
}
func (f *fakeDoge) unspentOutput(op wire.OutPoint) (bool, error) {
	all := append([]*wire.MsgTx{}, f.pool...)
	for _, b := range f.blocks {
		all = append(all, b...)
	}
	for _, tx := range all {
		for _, in := range tx.TxIn {
			if in.PreviousOutPoint == op {
				return false, nil
			}
		}
	}
	return true, nil
}
func (f *fakeDoge) mempool() ([]chainhash.Hash, error) {
	var ids []chainhash.Hash
	for _, tx := range f.pool {
		ids = append(ids, tx.TxHash())
	}
	return ids, nil
}

func balanceOf(x *dogeIndex, script []byte) (confirmed, pending int64, history int) {
	utxos, hist, pend := x.view(script)
	for _, u := range utxos {
		confirmed += u.value
	}
	return confirmed, pend, len(hist)
}

func TestDogeIndex(t *testing.T) {
	require := require.New(t)
	chain := newFakeDoge()
	path := filepath.Join(t.TempDir(), "index")
	x, err := openDogeIndex(path, chain)
	require.NoError(err)

	alice := destination{kind: destP2PKH, hash: [20]byte{1}}.pkScript()
	bob := destination{kind: destP2PKH, hash: [20]byte{2}}.pkScript()

	// Paid before the index or the address existed: not seen.
	chain.pay(nil, 5*doge, alice)
	chain.mine()
	require.NoError(x.sync())
	_, err = x.watch(alice)
	require.NoError(err)

	// A payment in the mempool is pending, then confirmed.
	in := chain.pay(nil, 100*doge, alice)
	require.NoError(x.refreshMempool())
	c, p, _ := balanceOf(x, alice)
	require.Equal(int64(0), c)
	require.Equal(int64(100*doge), p)
	chain.mine()
	require.NoError(x.sync())
	require.NoError(x.refreshMempool())
	c, p, n := balanceOf(x, alice)
	require.Equal(int64(100*doge), c)
	require.Zero(p)
	require.Equal(1, n)

	// Alice pays Bob 30 (not watched) with 70 change back to herself.
	spend := wire.NewMsgTx(1)
	spend.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: in.TxHash()}, []byte{1}, nil))
	spend.AddTxOut(wire.NewTxOut(30*doge, bob))
	spend.AddTxOut(wire.NewTxOut(70*doge, alice))
	chain.pool = append(chain.pool, spend)
	require.NoError(x.refreshMempool())
	c, p, _ = balanceOf(x, alice)
	require.Zero(c, "the spent output no longer counts")
	require.Equal(int64(-30*doge), p)
	chain.mine()
	require.NoError(x.sync())
	require.NoError(x.refreshMempool())
	c, _, n = balanceOf(x, alice)
	require.Equal(int64(70*doge), c)
	require.Equal(2, n)

	// Created and spent within one block.
	first := chain.pay(nil, 10*doge, alice)
	second := wire.NewMsgTx(1)
	second.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: first.TxHash()}, []byte{1}, nil))
	second.AddTxOut(wire.NewTxOut(10*doge, bob))
	chain.pool = append(chain.pool, second)
	chain.mine()
	require.NoError(x.sync())
	c, _, _ = balanceOf(x, alice)
	require.Equal(int64(70*doge), c)

	// A reorganisation that drops the last block's transactions back to the
	// mempool, then mines them again.
	late := chain.pay(nil, 1*doge, alice)
	chain.mine()
	require.NoError(x.sync())
	c, _, _ = balanceOf(x, alice)
	require.Equal(int64(71*doge), c)
	chain.reorg(2, true)
	require.NoError(x.sync())
	require.NoError(x.refreshMempool())
	c, p, _ = balanceOf(x, alice)
	require.Equal(int64(70*doge), c, "the reorganised block's payment is unconfirmed again")
	require.Equal(int64(1*doge), p)
	chain.mine()
	require.NoError(x.sync())
	require.NoError(x.refreshMempool())
	c, p, _ = balanceOf(x, alice)
	require.Equal(int64(71*doge), c)
	require.Zero(p)
	_ = late

	// The 5 DOGE paid before registration, added by transaction ID.
	var early chainhash.Hash
	for _, tx := range chain.blocks[1][1:] {
		early = tx.TxHash()
	}
	added, err := x.importTx(alice, early)
	require.NoError(err)
	require.Equal(1, added)
	c, _, _ = balanceOf(x, alice)
	require.Equal(int64(76*doge), c)
	_, err = x.importTx(alice, in.TxHash())
	require.ErrorContains(err, "no unspent output", "already spent")
	_, err = x.importTx(bob, early)
	require.ErrorContains(err, "not registered")

	// Reopening keeps everything, including which addresses are watched.
	require.NoError(x.close())
	x, err = openDogeIndex(path, chain)
	require.NoError(err)
	defer x.close()
	_, watched := x.isWatched(alice)
	require.True(watched)
	c, _, _ = balanceOf(x, alice)
	require.Equal(int64(76*doge), c)
}
