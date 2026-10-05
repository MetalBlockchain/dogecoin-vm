package main

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"

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

// conflictLister is a chain whose wallet can list the transactions that
// conflict with one in a block (chainTx.confirmations is minus that one's
// depth).
type conflictLister interface {
	conflictedFor(addresses []btcutil.Address) ([]chainTx, error)
}

// confirmer is a chain that can say whether a transaction is in a block.
type confirmer interface {
	confirmed(txid chainhash.Hash) (bool, error)
}

// confirmedOn returns a check of whether a transaction is in a block on c,
// or nil if c can't tell. An error counts as not known to be confirmed.
func confirmedOn(c chain) func(string) bool {
	cc, ok := c.(confirmer)
	if !ok {
		return nil
	}
	return func(txid string) bool {
		h, err := chainhash.NewHashFromStr(txid)
		if err != nil {
			return false
		}
		yes, err := cc.confirmed(*h)
		return err == nil && yes
	}
}

// utxoSetter is a chain whose node can say whether an output is still in
// its UTXO set: in a block and not spent by one.
type utxoSetter interface {
	inUTXOSet(op wire.OutPoint) (bool, error)
}

// unspentOn returns a check of whether an output is still in c's UTXO set,
// or nil if c can't tell. An error counts as still unspent: the checks that
// use it then refuse, which is the safe side.
func unspentOn(c chain) func(wire.OutPoint) bool {
	uc, ok := c.(utxoSetter)
	if !ok {
		return nil
	}
	return func(op wire.OutPoint) bool {
		yes, err := uc.inUTXOSet(op)
		return err != nil || yes
	}
}

// gettxoutInBlock asks a node's UTXO set (not a wallet) whether op is in a
// block and unspent by any block; a spend still in the mempool doesn't
// count.
func gettxoutInBlock(rpc *rpcClient, op wire.OutPoint) (bool, error) {
	var result *struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := rpc.call(&result, "gettxout", op.Hash.String(), op.Index, false); err != nil {
		return false, err
	}
	return result != nil && result.Confirmations > 0, nil
}

func (c *vmChain) inUTXOSet(op wire.OutPoint) (bool, error)   { return gettxoutInBlock(c.rpc, op) }
func (c *dogeChain) inUTXOSet(op wire.OutPoint) (bool, error) { return gettxoutInBlock(c.rpc, op) }

// confirmed reports whether DogecoinVM has txid in a block (its node keeps
// a transaction index).
func (c *vmChain) confirmed(txid chainhash.Hash) (bool, error) {
	var t struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := c.rpc.call(&t, "getrawtransaction", txid.String(), 1); err != nil {
		return false, err
	}
	return t.Confirmations > 0, nil
}

// confirmed reports whether Dogecoin has txid in a block (the node keeps a
// transaction index).
func (c *dogeChain) confirmed(txid chainhash.Hash) (bool, error) {
	var t struct {
		Confirmations int64 `json:"confirmations"`
	}
	if err := c.rpc.call(&t, "getrawtransaction", txid.String(), 1); err != nil {
		return false, err
	}
	return t.Confirmations > 0, nil
}

// resend hands tx back to the Dogecoin node; one it already has is fine.
func (c *dogeChain) resend(tx *wire.MsgTx) error {
	_, err := sendRaw(c.rpc, tx)
	if err != nil && (strings.Contains(err.Error(), "already") || strings.Contains(err.Error(), "txn-mempool-conflict")) {
		return nil
	}
	return err
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

// resend hands tx back to DogecoinVM's node; one it already has is fine.
func (c *vmChain) resend(tx *wire.MsgTx) error {
	_, err := sendRaw(c.rpc, tx)
	if err != nil && (strings.Contains(err.Error(), "already") || strings.Contains(err.Error(), "exists")) {
		return nil
	}
	return err
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

	mu    sync.Mutex
	hist  *walletHistory
	known map[string]*wire.MsgTx // wallet transactions read so far, by txid: their bytes never change
	// dead keeps unsettled transactions on disk (deadhistory.go); nil
	// keeps nothing (tests, and commands that read no cutover).
	dead *deadHistory
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
	tip, entries, err := c.history()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for txid := range entries {
		ids = append(ids, txid)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := entries[ids[i]], entries[ids[j]]
		if a.time != b.time {
			return a.time < b.time
		}
		return ids[i] < ids[j]
	})
	var txs []chainTx
	for _, txid := range ids {
		e := entries[txid]
		if e.conflicted {
			continue // conflicts with a transaction in a block: it won't confirm
		}
		tx, err := c.walletTx(txid)
		if err != nil {
			return nil, err
		}
		ok, err := touches(tx, scripts, c)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		var confirmations int64
		if e.height > 0 {
			confirmations = tip - e.height + 1
		}
		txs = append(txs, chainTx{tx: tx, confirmations: confirmations, time: e.time})
	}
	return txs, nil
}

// conflictedFor returns the wallet's transactions that pay to or spend from
// any of addresses and conflict with a transaction in a block: txsFor
// leaves them out, as they can't confirm unless a reorganisation undoes
// that block. Each one's confirmations is minus the depth of the block
// transaction it conflicts with. A cutover (cutover.go) counts their
// outputs too: a signature given before it may spend one, and it is dead
// only once that conflict is deep.
func (c *dogeChain) conflictedFor(addresses []btcutil.Address) ([]chainTx, error) {
	scripts := scriptSet(addresses)
	_, entries, err := c.history()
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	for txid, e := range entries {
		if e.conflicted {
			ids = append(ids, txid)
		}
	}
	sort.Strings(ids)
	var txs []chainTx
	for _, txid := range ids {
		tx, err := c.walletTx(txid)
		if err != nil {
			return nil, err
		}
		ok, err := touches(tx, scripts, c)
		if err != nil {
			return nil, err
		}
		if ok {
			e := entries[txid]
			txs = append(txs, chainTx{tx: tx, confirmations: -max(e.conflictDepth, 1), time: e.time})
		}
	}
	return txs, nil
}

// walletTx is wallet transaction txid, read from the node once.
func (c *dogeChain) walletTx(txid string) (*wire.MsgTx, error) {
	c.mu.Lock()
	tx, ok := c.known[txid]
	c.mu.Unlock()
	if ok {
		return tx, nil
	}
	var t struct {
		Hex string `json:"hex"`
	}
	if err := c.rpc.call(&t, "gettransaction", txid, true); err != nil {
		return nil, err
	}
	tx, err := decodeTx(t.Hex)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	if c.known == nil {
		c.known = map[string]*wire.MsgTx{}
	}
	c.known[txid] = tx
	c.mu.Unlock()
	return tx, nil
}

// walletHistory is this process's copy of the wallet's transaction list.
// Reading the whole list on every pass would cost time in proportion to the
// wallet's history, which anyone can lengthen by paying the peg's public
// addresses, and listing it in one call stops at a cap; so after the first
// read, each pass reads only what changed since the block the last one
// ended at (listsinceblock). Dogecoin Core 1.14 doesn't say which
// transactions a reorganisation took out of blocks, so when the block the
// last read ended at is no longer on the main chain, the whole list is read
// again; so is a wallet that gained transactions the read didn't show (an
// import of an old payment).
type walletHistory struct {
	last    string // the block the last read ended at
	entries map[string]historyEntry
}

type historyEntry struct {
	height     int64 // the block holding it; 0 if none
	conflicted bool  // conflicts with a transaction in a block
	// conflictDepth is, for a conflicted transaction, the confirmations of
	// the block transaction it conflicts with (Dogecoin Core lists it with
	// that many negative confirmations).
	conflictDepth int64
	time          int64
}

// history brings the wallet history up to date and returns the tip's height
// and a copy of every entry, with every unsettled transaction the wallet no
// longer lists added back as conflicted (deadhistory.go).
func (c *dogeChain) history() (int64, map[string]historyEntry, error) {
	tip, entries, err := c.walletEntries()
	if err != nil || c.dead == nil {
		return tip, entries, err
	}
	gone, err := c.dead.sync(tip, entries, func(txid string) (string, error) {
		tx, err := c.walletTx(txid)
		if err != nil {
			return "", err
		}
		return encodeTx(tx), nil
	})
	if err != nil {
		return 0, nil, err
	}
	// A dropped transaction comes back as conflicted at the current depth of
	// the best evidence it can never be mined (deadproof.go): a spend of one
	// of its inputs, from the wallet or an operator's proof, read fresh on
	// every read, as a reorganisation can make it shallower again. It stays
	// in the file and in history even once that evidence is rootDepth deep
	// (Round 8, X1): a cutover clears its recorded outputs only by seeing
	// that conflict (chainView.cleared), so deleting the record would leave
	// them owed for good. The file keeps it as a tombstone; a reorganisation
	// that makes the evidence shallow again shows it as owed again.
	// Dropped transactions added back below are never in a block, so the
	// wallet scan in droppedDepth skips them.
	for txid, rec := range gone {
		tx, err := decodeTx(rec.Hex)
		if err != nil {
			return 0, nil, fmt.Errorf("unsettled transaction %s: %w", txid, err)
		}
		depth, err := c.droppedDepth(tip, tx, entries, rec.Proof)
		if err != nil {
			return 0, nil, fmt.Errorf("unsettled transaction %s: %w", txid, err)
		}
		if rec.Settled {
			// The wallet once listed it conflicted rootDepth deep.
			depth = max(depth, c.dead.depth)
		}
		c.mu.Lock()
		if c.known == nil {
			c.known = map[string]*wire.MsgTx{}
		}
		c.known[txid] = tx
		c.mu.Unlock()
		entries[txid] = historyEntry{conflicted: true, conflictDepth: depth, time: rec.Time}
	}
	return tip, entries, nil
}

// walletEntries brings the wallet history up to date and returns the tip's
// height and a copy of every entry the wallet lists.
func (c *dogeChain) walletEntries() (int64, map[string]historyEntry, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tip, err := c.readHistory(false)
	if err != nil {
		return 0, nil, err
	}
	var info struct {
		TxCount int `json:"txcount"`
	}
	if err := c.rpc.call(&info, "getwalletinfo"); err != nil {
		return 0, nil, err
	}
	if info.TxCount != len(c.hist.entries) {
		if tip, err = c.readHistory(true); err != nil {
			return 0, nil, err
		}
	}
	entries := make(map[string]historyEntry, len(c.hist.entries))
	for txid, e := range c.hist.entries {
		entries[txid] = e
	}
	return tip, entries, nil
}

// readHistory reads what changed since the last read, or everything if
// full, there was none, or the block that read ended at is no longer on the
// main chain, and returns the height of the block it ended at.
func (c *dogeChain) readHistory(full bool) (int64, error) {
	if c.hist == nil {
		c.hist = &walletHistory{}
	}
	if !full && c.hist.last != "" {
		var at struct {
			Confirmations int64 `json:"confirmations"`
		}
		if err := c.rpc.call(&at, "getblockheader", c.hist.last); err != nil || at.Confirmations < 1 {
			full = true // reorganised away (or unreadable): start again
		}
	} else {
		full = true
	}
	from := c.hist.last
	if full {
		from = ""
	}
	var since struct {
		Transactions []struct {
			TxID          string `json:"txid"`
			Confirmations int64  `json:"confirmations"`
			Time          int64  `json:"time"`
		} `json:"transactions"`
		LastBlock string `json:"lastblock"`
	}
	// listsinceblock blockhash target_confirmations include_watchonly
	if err := c.rpc.call(&since, "listsinceblock", from, 1, true); err != nil {
		return 0, err
	}
	var header struct {
		Height int64 `json:"height"`
	}
	if err := c.rpc.call(&header, "getblockheader", since.LastBlock); err != nil {
		return 0, err
	}
	entries := c.hist.entries
	if full || entries == nil {
		entries = map[string]historyEntry{}
	}
	for _, e := range since.Transactions {
		h := historyEntry{conflicted: e.Confirmations < 0, time: e.Time}
		if h.conflicted {
			h.conflictDepth = -e.Confirmations
		}
		if e.Confirmations > 0 {
			h.height = header.Height - e.Confirmations + 1
		}
		entries[e.TxID] = h // one entry per output; they agree
	}
	c.hist.entries, c.hist.last = entries, since.LastBlock
	return header.Height, nil
}

// touches reports whether tx pays to one of scripts or spends an output
// paying to one. The wallet lists only transactions that do one or the
// other, so a transaction whose inputs can't be read fails rather than
// being dropped: dropping a peg payout would hide that it was paid.
func touches(tx *wire.MsgTx, scripts map[string]bool, c *dogeChain) (bool, error) {
	for _, out := range tx.TxOut {
		if scripts[string(out.PkScript)] {
			return true, nil
		}
	}
	for _, in := range tx.TxIn {
		if in.PreviousOutPoint.Hash == (chainhash.Hash{}) {
			continue // a coinbase
		}
		// Needs Dogecoin Core's -txindex for outputs the wallet did not
		// create.
		var prevHex string
		if err := c.rpc.call(&prevHex, "getrawtransaction", in.PreviousOutPoint.Hash.String(), 0); err != nil {
			return false, fmt.Errorf("reading %v, which wallet transaction %v spends (is -txindex on?): %w", in.PreviousOutPoint.Hash, tx.TxHash(), err)
		}
		prevTx, err := decodeTx(prevHex)
		if err != nil {
			return false, err
		}
		if int(in.PreviousOutPoint.Index) < len(prevTx.TxOut) &&
			scripts[string(prevTx.TxOut[in.PreviousOutPoint.Index].PkScript)] {
			return true, nil
		}
	}
	return false, nil
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
