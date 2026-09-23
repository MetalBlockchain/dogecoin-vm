package vm

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	btcd "github.com/MetalBlockchain/btcvm/btcd"
	"github.com/MetalBlockchain/btcvm/btcd/blockchain"
	"github.com/MetalBlockchain/btcvm/btcd/btcec/v2"
	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/txscript"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// pegSigners is a 2-of-3 multisig standing in for the peg signers.
type pegSigners struct {
	keys         []*btcec.PrivateKey
	redeemScript []byte
	address      *btcutil.AddressScriptHash
	pkScript     []byte
}

func newPegSigners(t *testing.T, params *chaincfg.Params) *pegSigners {
	t.Helper()
	require := require.New(t)

	s := &pegSigners{}
	var pubKeys []*btcutil.AddressPubKey
	for i := byte(1); i <= 3; i++ {
		key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{i}, 32))
		s.keys = append(s.keys, key)
		pub, err := btcutil.NewAddressPubKey(key.PubKey().SerializeCompressed(), params)
		require.NoError(err)
		pubKeys = append(pubKeys, pub)
	}

	var err error
	s.redeemScript, err = txscript.MultiSigScript(pubKeys, 2)
	require.NoError(err)
	s.address, err = btcutil.NewAddressScriptHash(s.redeemScript, params)
	require.NoError(err)
	s.pkScript, err = txscript.PayToAddrScript(s.address)
	require.NoError(err)
	return s
}

// sign signs input idx of tx, which spends a reserve output, with the first
// two signers.
func (s *pegSigners) sign(t *testing.T, params *chaincfg.Params, tx *wire.MsgTx, idx int) {
	t.Helper()
	keys := map[string]*btcec.PrivateKey{}
	for _, key := range s.keys[:2] {
		pub, err := btcutil.NewAddressPubKey(key.PubKey().SerializeCompressed(), params)
		require.NoError(t, err)
		keys[pub.EncodeAddress()] = key
	}
	sigScript, err := txscript.SignTxOutput(params, tx, idx, s.pkScript, txscript.SigHashAll,
		txscript.KeyClosure(func(addr btcutil.Address) (*btcec.PrivateKey, bool, error) {
			key, ok := keys[addr.EncodeAddress()]
			if !ok {
				return nil, false, errors.New("not a signer")
			}
			return key, true, nil
		}),
		txscript.ScriptClosure(func(btcutil.Address) ([]byte, error) { return s.redeemScript, nil }),
		nil)
	require.NoError(t, err)
	tx.TxIn[idx].SignatureScript = sigScript
}

func setupPegVM(t *testing.T, blocks int32) (*VM, *pegSigners) {
	t.Helper()
	signers := newPegSigners(t, &btcd.DogecoinVMTestNetParams)
	vm := setupVMWithConfig(t, map[string]any{
		"pegReserveAddress": signers.address.EncodeAddress(),
		"pegReserveBlocks":  blocks,
	})
	return vm, signers
}

// reserveOutput returns the index of the peg reserve output in blk's
// coinbase, or -1.
func reserveOutput(blk *BlockAdapter, signers *pegSigners) int {
	for i, out := range blk.btcBlock.MsgBlock().Transactions[0].TxOut {
		if bytes.Equal(out.PkScript, signers.pkScript) {
			return i
		}
	}
	return -1
}

func TestPegReserveCreatedInFirstBlocks(t *testing.T) {
	require := require.New(t)
	vm, signers := setupPegVM(t, 2)

	for height := 1; height <= 3; height++ {
		blk := buildBlock(t, vm)
		idx := reserveOutput(blk, signers)
		if height <= 2 {
			require.GreaterOrEqual(idx, 0, "height %d has no reserve output", height)
			out := blk.btcBlock.MsgBlock().Transactions[0].TxOut[idx]
			require.Equal(int64(btcd.PegReserveAmountPerBlock), out.Value)
		} else {
			require.Equal(-1, idx, "height %d pays the reserve", height)
		}
		require.NoError(blk.Verify(context.Background()))
		require.NoError(blk.Accept(context.Background()))
	}
}

func TestPegReserveCannotBeRedirected(t *testing.T) {
	vm, signers := setupPegVM(t, 1)
	blk := buildBlock(t, vm)
	idx := reserveOutput(blk, signers)
	require.GreaterOrEqual(t, idx, 0)

	tests := []struct {
		name     string
		mutate   func(*wire.MsgBlock)
		wantCode blockchain.ErrorCode
	}{
		{
			name: "reserve paid to the block builder",
			mutate: func(msg *wire.MsgBlock) {
				coinbase := msg.Transactions[0]
				coinbase.TxOut[0].Value += coinbase.TxOut[idx].Value
				coinbase.TxOut = append(coinbase.TxOut[:idx], coinbase.TxOut[idx+1:]...)
			},
			wantCode: blockchain.ErrBadPegReserve,
		},
		{
			name: "reserve short by one koinu",
			mutate: func(msg *wire.MsgBlock) {
				msg.Transactions[0].TxOut[idx].Value--
				msg.Transactions[0].TxOut[0].Value++
			},
			wantCode: blockchain.ErrBadPegReserve,
		},
		{
			name: "coinbase mints more than the reserve",
			mutate: func(msg *wire.MsgBlock) {
				msg.Transactions[0].TxOut[0].Value++
			},
			wantCode: blockchain.ErrBadCoinbaseValue,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b := parse(t, vm, mutateBlock(t, blk, 1, 7, test.mutate))
			err := b.Verify(context.Background())
			var ruleErr blockchain.RuleError
			require.True(t, errors.As(err, &ruleErr), "got %v", err)
			require.Equal(t, test.wantCode, ruleErr.ErrorCode)
		})
	}
}

// TestPegReserveRelease spends reserve coins with the peg signers' multisig,
// as a peg-in does, and checks the release confirms.
func TestPegReserveRelease(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	vm, signers := setupPegVM(t, 1)
	params := vm.config.ChainParams

	blk := buildBlock(t, vm)
	require.NoError(blk.Verify(ctx))
	require.NoError(blk.Accept(ctx))
	coinbase := blk.btcBlock.Transactions()[0]
	idx := reserveOutput(blk, signers)

	user, err := btcutil.NewAddressPubKeyHash(bytes.Repeat([]byte{0x77}, 20), params)
	require.NoError(err)
	userScript, err := txscript.PayToAddrScript(user)
	require.NoError(err)

	const credit = 1_000 * 1e8 // 1,000 DOGE
	const fee = 1e6            // 0.01 DOGE
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(wire.NewTxIn(wire.NewOutPoint(coinbase.Hash(), uint32(idx)), nil, nil))
	tx.AddTxOut(wire.NewTxOut(credit, userScript))
	tx.AddTxOut(wire.NewTxOut(btcd.PegReserveAmountPerBlock-credit-fee, signers.pkScript))
	signers.sign(t, params, tx, 0)

	_, err = vm.btcdAdapter.TxMemPool().ProcessTransaction(btcutil.NewTx(tx), false, false, 0)
	require.NoError(err)

	next := buildBlock(t, vm)
	require.Len(next.btcBlock.Transactions(), 2)
	require.NoError(next.Verify(ctx))
	require.NoError(next.Accept(ctx))

	entry, err := vm.chain.FetchUtxoEntry(wire.OutPoint{Hash: tx.TxHash(), Index: 0})
	require.NoError(err)
	require.NotNil(entry)
	require.Equal(int64(credit), entry.Amount())
}

func TestPegReserveConfigValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	signers := newPegSigners(t, &btcd.DogecoinVMMainNetParams) // wrong network

	for name, extra := range map[string]map[string]any{
		"address without blocks":      {"pegReserveAddress": signers.address.EncodeAddress()},
		"blocks without address":      {"pegReserveBlocks": 1},
		"address for another network": {"pegReserveAddress": signers.address.EncodeAddress(), "pegReserveBlocks": 1},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := startTestVM(t, t.TempDir(), extra)
			require.Error(t, err)
		})
	}
}

func TestApplyChainConfig(t *testing.T) {
	require := require.New(t)

	cfg := btcd.Config{TestNet: true, RPCUser: "genesis", DataDir: "/genesis"}
	require.NoError(applyChainConfig(&cfg, []byte(`{"rpcUser":"node","addrIndex":true}`)))
	require.Equal("node", cfg.RPCUser)
	require.True(cfg.AddrIndex)
	require.Equal("/genesis", cfg.DataDir, "absent keys keep their genesis value")
	require.True(cfg.TestNet)

	for _, key := range consensusConfigKeys {
		err := applyChainConfig(&cfg, []byte(`{"`+key+`":true}`))
		require.ErrorContains(err, "consensus setting", key)
	}
}
