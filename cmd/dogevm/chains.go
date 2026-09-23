package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// chainTx is a transaction and how many blocks confirm it (0 in mempool).
type chainTx struct {
	tx            *wire.MsgTx
	confirmations int64
	time          int64 // unix seconds: block time, or when first seen
}

// utxo is an unspent output.
type utxo struct {
	outPoint      wire.OutPoint
	value         int64
	pkScript      []byte
	confirmations int64
}

// chain is what the bridge and wallet need from either ledger.
type chain interface {
	// txsFor returns every transaction, confirmed or in the mempool, that
	// pays to or spends from any of addresses.
	txsFor(addresses []btcutil.Address) ([]chainTx, error)
	// unspent returns the addresses' unspent outputs with at least minConf
	// confirmations.
	unspent(addresses []btcutil.Address, minConf int64) ([]utxo, error)
	// send broadcasts tx.
	send(tx *wire.MsgTx) (chainhash.Hash, error)
}

func decodeTx(hexTx string) (*wire.MsgTx, error) {
	raw, err := hex.DecodeString(hexTx)
	if err != nil {
		return nil, err
	}
	var tx wire.MsgTx
	if err := tx.Deserialize(bytes.NewReader(raw)); err != nil {
		return nil, err
	}
	return &tx, nil
}

func encodeTx(tx *wire.MsgTx) string {
	var buf bytes.Buffer
	_ = tx.Serialize(&buf)
	return hex.EncodeToString(buf.Bytes())
}

// vmChain reads DogecoinVM through its btcd JSON-RPC. The node must run with
// txIndex and addrIndex enabled.
type vmChain struct {
	rpc *rpcClient
}

func (c *vmChain) txsFor(addresses []btcutil.Address) ([]chainTx, error) {
	seen := map[chainhash.Hash]bool{}
	var txs []chainTx
	for _, address := range addresses {
		found, err := c.addressTxs(address)
		if err != nil {
			return nil, err
		}
		for _, t := range found {
			if hash := t.tx.TxHash(); !seen[hash] {
				seen[hash] = true
				txs = append(txs, t)
			}
		}
	}
	return txs, nil
}

func (c *vmChain) addressTxs(address btcutil.Address) ([]chainTx, error) {
	const pageSize = 500
	var txs []chainTx
	for skip := 0; ; skip += pageSize {
		var page []struct {
			Hex           string `json:"hex"`
			Confirmations int64  `json:"confirmations"`
			Time          int64  `json:"time"`
		}
		err := c.rpc.call(&page, "searchrawtransactions", address.EncodeAddress(), 1, skip, pageSize)
		if isRPCCode(err, errNoAddressInfo) {
			return txs, nil
		}
		if err != nil {
			return nil, err
		}
		for _, r := range page {
			tx, err := decodeTx(r.Hex)
			if err != nil {
				return nil, err
			}
			txs = append(txs, chainTx{tx: tx, confirmations: r.Confirmations, time: r.Time})
		}
		if len(page) < pageSize {
			return txs, nil
		}
	}
}

func (c *vmChain) unspent(addresses []btcutil.Address, minConf int64) ([]utxo, error) {
	txs, err := c.txsFor(addresses)
	if err != nil {
		return nil, err
	}
	scripts := scriptSet(addresses)

	var utxos []utxo
	for _, t := range txs {
		if t.confirmations < minConf {
			continue
		}
		hash := t.tx.TxHash()
		for i, out := range t.tx.TxOut {
			if !scripts[string(out.PkScript)] {
				continue
			}
			// gettxout returns null once the output is spent, including
			// by a transaction still in the mempool.
			var result *struct {
				Confirmations int64 `json:"confirmations"`
			}
			if err := c.rpc.call(&result, "gettxout", hash.String(), i, true); err != nil {
				return nil, err
			}
			if result == nil {
				continue
			}
			utxos = append(utxos, utxo{
				outPoint:      wire.OutPoint{Hash: hash, Index: uint32(i)},
				value:         out.Value,
				pkScript:      out.PkScript,
				confirmations: t.confirmations,
			})
		}
	}
	return utxos, nil
}

func (c *vmChain) send(tx *wire.MsgTx) (chainhash.Hash, error) {
	return sendRaw(c.rpc, tx)
}

func sendRaw(rpc *rpcClient, tx *wire.MsgTx) (chainhash.Hash, error) {
	var txid string
	if err := rpc.call(&txid, "sendrawtransaction", encodeTx(tx)); err != nil {
		return chainhash.Hash{}, err
	}
	hash, err := chainhash.NewHashFromStr(txid)
	if err != nil {
		return chainhash.Hash{}, err
	}
	return *hash, nil
}

// dogeChain reads Dogecoin through Dogecoin Core's JSON-RPC. Addresses must
// be imported into its wallet (watch-only is enough) with importaddress.
type dogeChain struct {
	rpc *rpcClient
}

// importTx adds one transaction, already in a block, to the wallet without a
// rescan: this node proves it is in the chain (gettxoutproof) and the wallet
// takes it (importprunedfunds). It lets a signer that started watching an
// address late see a payment made to it before then.
func (c *dogeChain) importTx(txid chainhash.Hash) error {
	var raw string
	if err := c.rpc.call(&raw, "getrawtransaction", txid.String(), 0); err != nil {
		return err
	}
	var proof string
	if err := c.rpc.call(&proof, "gettxoutproof", []string{txid.String()}); err != nil {
		return err
	}
	return c.rpc.call(nil, "importprunedfunds", raw, proof)
}

// watch imports address into the Dogecoin Core wallet as watch-only.
func (c *dogeChain) watch(address btcutil.Address, rescan bool) error {
	return c.rpc.call(nil, "importaddress", address.EncodeAddress(), "dogevm", rescan)
}

func (c *dogeChain) txsFor(addresses []btcutil.Address) ([]chainTx, error) {
	scripts := scriptSet(addresses)
	var entries []struct {
		TxID    string `json:"txid"`
		Address string `json:"address"`
	}
	// listtransactions account count skip include_watchonly
	if err := c.rpc.call(&entries, "listtransactions", "*", 1_000_000, 0, true); err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var txs []chainTx
	for _, e := range entries {
		if seen[e.TxID] {
			continue
		}
		seen[e.TxID] = true

		var t struct {
			Hex           string `json:"hex"`
			Confirmations int64  `json:"confirmations"`
			Time          int64  `json:"time"`
		}
		if err := c.rpc.call(&t, "gettransaction", e.TxID, true); err != nil {
			return nil, err
		}
		tx, err := decodeTx(t.Hex)
		if err != nil {
			return nil, err
		}
		if touches(tx, scripts, c) {
			txs = append(txs, chainTx{tx: tx, confirmations: t.Confirmations, time: t.Time})
		}
	}
	return txs, nil
}

// touches reports whether tx pays to one of scripts or spends an output
// paying to one.
func touches(tx *wire.MsgTx, scripts map[string]bool, c *dogeChain) bool {
	for _, out := range tx.TxOut {
		if scripts[string(out.PkScript)] {
			return true
		}
	}
	for _, in := range tx.TxIn {
		// Needs Dogecoin Core's -txindex for outputs the wallet did not
		// create.
		var prevHex string
		if err := c.rpc.call(&prevHex, "getrawtransaction", in.PreviousOutPoint.Hash.String(), 0); err != nil {
			continue
		}
		prevTx, err := decodeTx(prevHex)
		if err == nil && int(in.PreviousOutPoint.Index) < len(prevTx.TxOut) &&
			scripts[string(prevTx.TxOut[in.PreviousOutPoint.Index].PkScript)] {
			return true
		}
	}
	return false
}

func (c *dogeChain) unspent(addresses []btcutil.Address, minConf int64) ([]utxo, error) {
	encoded := make([]string, len(addresses))
	for i, a := range addresses {
		encoded[i] = a.EncodeAddress()
	}
	var entries []struct {
		TxID          string  `json:"txid"`
		Vout          uint32  `json:"vout"`
		Amount        float64 `json:"amount"`
		ScriptPubKey  string  `json:"scriptPubKey"`
		Confirmations int64   `json:"confirmations"`
	}
	err := c.rpc.call(&entries, "listunspent", minConf, 9_999_999, encoded)
	if err != nil {
		return nil, err
	}
	var utxos []utxo
	for _, e := range entries {
		hash, err := chainhash.NewHashFromStr(e.TxID)
		if err != nil {
			return nil, err
		}
		script, err := hex.DecodeString(e.ScriptPubKey)
		if err != nil {
			return nil, err
		}
		utxos = append(utxos, utxo{
			outPoint: wire.OutPoint{Hash: *hash, Index: e.Vout},
			// Dogecoin Core reports amounts as JSON numbers in DOGE.
			value:         int64(math.Round(e.Amount * koinuPerDoge)),
			pkScript:      script,
			confirmations: e.Confirmations,
		})
	}
	return utxos, nil
}

// sender returns the destination that funded the first input of txid, which
// is where a refund of it should normally go.
func (c *dogeChain) sender(txid chainhash.Hash) (destination, error) {
	var txHex string
	if err := c.rpc.call(&txHex, "getrawtransaction", txid.String(), 0); err != nil {
		return destination{}, err
	}
	tx, err := decodeTx(txHex)
	if err != nil {
		return destination{}, err
	}
	prev := tx.TxIn[0].PreviousOutPoint
	var prevHex string
	if err := c.rpc.call(&prevHex, "getrawtransaction", prev.Hash.String(), 0); err != nil {
		return destination{}, err
	}
	prevTx, err := decodeTx(prevHex)
	if err != nil {
		return destination{}, err
	}
	if int(prev.Index) >= len(prevTx.TxOut) {
		return destination{}, errors.New("malformed input")
	}
	return destinationOfScript(prevTx.TxOut[prev.Index].PkScript)
}

func (c *dogeChain) send(tx *wire.MsgTx) (chainhash.Hash, error) {
	return sendRaw(c.rpc, tx)
}

// scriptSet returns the output scripts paying to addresses, as map keys.
func scriptSet(addresses []btcutil.Address) map[string]bool {
	scripts := make(map[string]bool, len(addresses))
	for _, a := range addresses {
		scripts[string(destinationScript(a))] = true
	}
	return scripts
}

// destinationScript returns the output script paying to address.
func destinationScript(address btcutil.Address) []byte {
	d, err := destinationOf(address)
	if err != nil {
		panic(fmt.Sprintf("destinationScript: %v", err))
	}
	return d.pkScript()
}
