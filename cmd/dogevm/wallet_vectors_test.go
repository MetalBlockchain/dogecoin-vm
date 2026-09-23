package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
)

// walletVectors is testdata/wallet-vectors.json: what any wallet for these
// chains must reproduce byte for byte (the web wallet made them; the macOS
// app's core is tested against them).
type walletVectors struct {
	Keys []struct {
		Label, Hash160, Address, Wif string
	}
	Deposit struct {
		Signers struct {
			Required   int
			PublicKeys []string
		}
		Dest         struct{ Hash160 string }
		RedeemScript string
		Address      string
	}
	ReserveScript string
	Payments      []struct {
		Name, FromLabel, PrevTx, ToScript, Amount, Data, Tx, Txid, Fee string
	}
}

// TestWalletVectors regenerates the vectors from the web wallet, checks them
// against the Go code and btcd's script engine, and checks the committed
// file is current.
func TestWalletVectors(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	require := require.New(t)
	fresh, err := exec.Command(node, "testdata/wallet_vectors.mjs").Output()
	require.NoError(err)
	committed, err := os.ReadFile("testdata/wallet-vectors.json")
	require.NoError(err, "run: node testdata/wallet_vectors.mjs > testdata/wallet-vectors.json")
	require.Equal(string(bytes.TrimSpace(committed)), string(bytes.TrimSpace(fresh)),
		"testdata/wallet-vectors.json is stale; run: node testdata/wallet_vectors.mjs > testdata/wallet-vectors.json")

	var v walletVectors
	require.NoError(json.Unmarshal(fresh, &v))
	vmMain, err := dogevmParams("mainnet")
	require.NoError(err)

	keys := map[string]*btcec.PrivateKey{}
	for _, k := range v.Keys {
		secret := sha256.Sum256([]byte(k.Label))
		key, _ := btcec.PrivKeyFromBytes(secret[:])
		keys[k.Label] = key
		addr, err := p2pkhAddress(key, vmMain)
		require.NoError(err)
		require.Equal(k.Address, addr.EncodeAddress(), k.Label)
		dogeAddr, err := p2pkhAddress(key, &dogecoinMainNet)
		require.NoError(err)
		require.Equal(k.Address, dogeAddr.EncodeAddress(), "one address on both networks")
		wif, err := btcutil.NewWIF(key, &dogecoinMainNet, true)
		require.NoError(err)
		require.Equal(k.Wif, wif.String())
	}

	signers := &signerSet{Required: v.Deposit.Signers.Required, PublicKeys: v.Deposit.Signers.PublicKeys}
	require.NoError(signers.load())
	var dest destination
	h, _ := hex.DecodeString(v.Deposit.Dest.Hash160)
	copy(dest.hash[:], h)
	require.Equal(v.Deposit.RedeemScript, hex.EncodeToString(signers.depositRedeemScript(dest)))
	depositAddr, err := signers.depositAddress(dest, &dogecoinMainNet)
	require.NoError(err)
	require.Equal(v.Deposit.Address, depositAddr.EncodeAddress())
	require.Equal(v.ReserveScript, hex.EncodeToString(signers.pkScript()))

	for _, p := range v.Payments {
		prev, err := decodeTx(p.PrevTx)
		require.NoError(err, p.Name)
		tx, err := decodeTx(p.Tx)
		require.NoError(err, p.Name)
		require.Equal(p.Txid, tx.TxHash().String(), p.Name)
		var in, out int64
		for i, txIn := range tx.TxIn {
			require.Equal(prev.TxHash(), txIn.PreviousOutPoint.Hash, p.Name)
			prevOut := prev.TxOut[txIn.PreviousOutPoint.Index]
			in += prevOut.Value
			engine, err := txscript.NewEngine(prevOut.PkScript, tx, i, txscript.StandardVerifyFlags, nil, nil,
				prevOut.Value, txscript.NewCannedPrevOutputFetcher(prevOut.PkScript, prevOut.Value))
			require.NoError(err, p.Name)
			require.NoError(engine.Execute(), "%s: input %d", p.Name, i)
		}
		for _, o := range tx.TxOut {
			out += o.Value
		}
		require.Equal(p.ToScript, hex.EncodeToString(tx.TxOut[0].PkScript), p.Name)
		require.Equal(p.Amount, strconv.FormatInt(tx.TxOut[0].Value, 10), p.Name)
		require.Equal(p.Fee, strconv.FormatInt(in-out, 10), p.Name)
	}
}
