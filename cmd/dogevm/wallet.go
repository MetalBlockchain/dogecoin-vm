package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/MetalBlockchain/btcvm/btcd/btcec/v2"
	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/btcvm/btcd/txscript"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
)

// walletFeePerByte is Dogecoin's recommended 0.01 DOGE/kB, ten times the
// minimum relay fee, so wallet transactions relay on either chain.
const walletFeePerByte = 1000

// softDust is Dogecoin's soft dust limit: outputs below it cost an extra
// 0.01 DOGE in fees, so the wallet never creates change that small.
const softDust = koinuPerDoge / 100

func newKey() (*btcec.PrivateKey, error) {
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return nil, err
	}
	key, _ := btcec.PrivKeyFromBytes(secret[:])
	return key, nil
}

// keyReport shows one key in both chains' encodings.
type keyReport struct {
	PrivateKeyHex  string `json:"privateKeyHex"`
	DogecoinVMWIF  string `json:"dogecoinvmWIF"`
	DogecoinVMAddr string `json:"dogecoinvmAddress"`
	DogecoinWIF    string `json:"dogecoinWIF"`
	DogecoinAddr   string `json:"dogecoinAddress"`
}

func describeKey(key *btcec.PrivateKey, vmParams, dogeParams *chaincfg.Params) (keyReport, error) {
	r := keyReport{PrivateKeyHex: hex.EncodeToString(key.Serialize())}
	for _, enc := range []struct {
		params    *chaincfg.Params
		wif, addr *string
	}{
		{vmParams, &r.DogecoinVMWIF, &r.DogecoinVMAddr},
		{dogeParams, &r.DogecoinWIF, &r.DogecoinAddr},
	} {
		wif, err := btcutil.NewWIF(key, enc.params, true)
		if err != nil {
			return r, err
		}
		addr, err := p2pkhAddress(key, enc.params)
		if err != nil {
			return r, err
		}
		*enc.wif, *enc.addr = wif.String(), addr.EncodeAddress()
	}
	return r, nil
}

func p2pkhAddress(key *btcec.PrivateKey, params *chaincfg.Params) (*btcutil.AddressPubKeyHash, error) {
	return btcutil.NewAddressPubKeyHash(btcutil.Hash160(key.PubKey().SerializeCompressed()), params)
}

// parseKey accepts a WIF for either chain or a hex private key.
func parseKey(s string) (*btcec.PrivateKey, error) {
	if wif, err := btcutil.DecodeWIF(s); err == nil {
		return wif.PrivKey, nil
	}
	raw, err := hex.DecodeString(s)
	if err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("key must be a WIF or 32-byte hex private key")
	}
	key, _ := btcec.PrivKeyFromBytes(raw)
	return key, nil
}

// estimateSize is a conservative size for a transaction spending P2PKH
// inputs, used to set its fee before signing.
func estimateSize(inputs, outputs int, opReturnBytes int) int64 {
	size := 10 + 149*inputs + 34*outputs
	if opReturnBytes > 0 {
		size += 11 + opReturnBytes
	}
	return int64(size)
}

// payFromKey sends amount to script from key's P2PKH outputs on c, adding
// extra (an OP_RETURN) if given, with change back to the key.
func payFromKey(c chain, params *chaincfg.Params, key *btcec.PrivateKey,
	script []byte, amount int64, extra *wire.TxOut) (chainhash.Hash, error) {

	from, err := p2pkhAddress(key, params)
	if err != nil {
		return chainhash.Hash{}, err
	}
	utxos, err := c.unspent([]btcutil.Address{from}, 1)
	if err != nil {
		return chainhash.Hash{}, err
	}

	opReturnBytes := 0
	if extra != nil {
		opReturnBytes = len(extra.PkScript)
	}
	// Grow the input set until it covers the amount and its own fee.
	var inputs []utxo
	var total, fee int64
	for n := 1; ; n++ {
		fee = estimateSize(n, 2, opReturnBytes) * walletFeePerByte
		inputs, total, err = selectUTXOs(utxos, amount+fee)
		if err != nil {
			return chainhash.Hash{}, fmt.Errorf("%s: %w", from.EncodeAddress(), err)
		}
		if len(inputs) <= n {
			break
		}
	}

	tx := wire.NewMsgTx(1)
	for _, u := range inputs {
		tx.AddTxIn(wire.NewTxIn(&u.outPoint, nil, nil))
	}
	tx.AddTxOut(wire.NewTxOut(amount, script))
	if extra != nil {
		tx.AddTxOut(extra)
	}
	if change := total - amount - fee; change >= softDust {
		tx.AddTxOut(wire.NewTxOut(change, destinationScript(from)))
	}

	fromScript := destinationScript(from)
	for i := range tx.TxIn {
		sig, err := txscript.SignatureScript(tx, i, fromScript, txscript.SigHashAll, key, true)
		if err != nil {
			return chainhash.Hash{}, err
		}
		tx.TxIn[i].SignatureScript = sig
	}
	return c.send(tx)
}

// balance sums address's unspent outputs.
func balance(c chain, address btcutil.Address) (confirmed, pending int64, err error) {
	utxos, err := c.unspent([]btcutil.Address{address}, 0)
	if err != nil {
		return 0, 0, err
	}
	for _, u := range utxos {
		if u.confirmations > 0 {
			confirmed += u.value
		} else {
			pending += u.value
		}
	}
	return confirmed, pending, nil
}

// depositFromDogecoinCore sends amount from the Dogecoin Core wallet behind
// rpc to the peg address, tagged to credit dest on DogecoinVM.
func depositFromDogecoinCore(rpc *rpcClient, pegAddr btcutil.Address, dest destination, amount int64) (string, error) {
	outputs := map[string]any{
		pegAddr.EncodeAddress(): json.Number(formatDoge(amount)),
		"data":                  hex.EncodeToString(encodeDestination(tagDeposit, dest)),
	}
	var raw string
	if err := rpc.call(&raw, "createrawtransaction", []any{}, outputs); err != nil {
		return "", err
	}
	var funded struct {
		Hex string `json:"hex"`
	}
	if err := rpc.call(&funded, "fundrawtransaction", raw); err != nil {
		return "", err
	}
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := rpc.call(&signed, "signrawtransaction", funded.Hex); err != nil {
		return "", err
	}
	if !signed.Complete {
		return "", fmt.Errorf("Dogecoin Core could not sign the deposit (is its wallet unlocked?)")
	}
	var txid string
	err := rpc.call(&txid, "sendrawtransaction", signed.Hex)
	return txid, err
}
