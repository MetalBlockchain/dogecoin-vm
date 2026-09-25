package main

// The web wallet's Dogecoin side: balances, history and sending for the
// user's own Dogecoin address, from the same key as their DogecoinVM
// address. Addresses are registered with the index (dogeindex.go) first.

import (
	"encoding/hex"
	"log"
	"net/http"
	"strconv"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
)

func (srv *server) dogeAddress(s string) (btcutil.Address, []byte, error) {
	addr, err := btcutil.DecodeAddress(s, srv.b.dogeParams)
	if err != nil || !addr.IsForNet(srv.b.dogeParams) {
		return nil, nil, badRequest("not a Dogecoin %s address: %q", srv.b.dogeParams.Name, s)
	}
	dest, err := destinationOf(addr)
	if err != nil {
		return nil, nil, badRequest("%v", err)
	}
	return addr, dest.pkScript(), nil
}

func (srv *server) requireIndex() error {
	if srv.dogeIdx == nil {
		return &apiError{http.StatusNotFound, "this bridge does not serve Dogecoin balances"}
	}
	if tip, _ := srv.dogeIdx.tip(); tip < 0 {
		return &apiError{http.StatusServiceUnavailable, "the bridge's Dogecoin node is still catching up; Dogecoin balances appear once it has"}
	}
	return nil
}

// dogeWatch registers a Dogecoin address with the index.
func (srv *server) dogeWatch(r *http.Request) (any, error) {
	if err := srv.requireIndex(); err != nil {
		return nil, err
	}
	var body struct {
		Address string `json:"address"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	addr, script, err := srv.dogeAddress(body.Address)
	if err != nil {
		return nil, err
	}
	if _, watched := srv.dogeIdx.isWatched(script); !watched && !srv.registerLimit.allow(clientIP(r)) {
		return nil, &apiError{http.StatusTooManyRequests, "too many new addresses from this IP; try later"}
	}
	from, err := srv.dogeIdx.watch(script)
	if err != nil {
		return nil, err
	}
	return map[string]any{"address": addr.EncodeAddress(), "watchedFrom": from}, nil
}

// dogeAddressHandler returns a registered address's balance, unspent outputs
// and history.
func (srv *server) dogeAddressHandler(r *http.Request) (any, error) {
	if err := srv.requireIndex(); err != nil {
		return nil, err
	}
	addr, script, err := srv.dogeAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	from, watched := srv.dogeIdx.isWatched(script)
	if !watched {
		return nil, &apiError{http.StatusNotFound, "address is not registered"}
	}
	tip, _ := srv.dogeIdx.tip()
	confs := func(height int64) int64 {
		if height == 0 {
			return 0
		}
		return tip - height + 1
	}
	utxos, history, pending := srv.dogeIdx.view(script)
	var confirmed int64
	outs := []map[string]any{}
	for _, u := range utxos {
		confirmed += u.value
		outs = append(outs, map[string]any{
			"txid": u.op.Hash.String(), "vout": u.op.Index,
			"value": strconv.FormatInt(u.value, 10), "script": hex.EncodeToString(script),
			"confirmations": confs(u.height),
		})
	}
	hist := []map[string]any{}
	for _, h := range history {
		hist = append(hist, map[string]any{
			"txid": h.txid.String(), "net": formatSigned(h.net),
			"confirmations": confs(h.height), "time": h.unix,
		})
	}
	return map[string]any{
		"address":     addr.EncodeAddress(),
		"confirmed":   formatDoge(confirmed),
		"pending":     formatSigned(pending),
		"utxos":       outs,
		"history":     hist,
		"watchedFrom": from,
		"indexedTo":   tip,
	}, nil
}

// dogeImport adds a payment made to a registered address before it was
// registered.
func (srv *server) dogeImport(r *http.Request) (any, error) {
	if err := srv.requireIndex(); err != nil {
		return nil, err
	}
	var body struct {
		Address string `json:"address"`
		Txid    string `json:"txid"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	_, script, err := srv.dogeAddress(body.Address)
	if err != nil {
		return nil, err
	}
	txid, err := chainhash.NewHashFromStr(body.Txid)
	if err != nil {
		return nil, badRequest("invalid transaction ID")
	}
	n, err := srv.dogeIdx.importTx(script, *txid)
	if err != nil {
		return nil, badRequest("%v", err)
	}
	return map[string]any{"imported": n}, nil
}

// dogeRawTx returns a Dogecoin transaction's bytes, so the wallet can check
// what it spends.
func (srv *server) dogeRawTx(r *http.Request) (any, error) {
	txid, err := chainhash.NewHashFromStr(r.PathValue("txid"))
	if err != nil {
		return nil, badRequest("invalid txid")
	}
	var hexTx string
	if err := srv.doge.rpc.call(&hexTx, "getrawtransaction", txid.String(), 0); err != nil {
		return nil, &apiError{http.StatusNotFound, "no such transaction on Dogecoin"}
	}
	return map[string]string{"hex": hexTx}, nil
}

// dogeBroadcast sends a signed Dogecoin transaction through the bridge's
// node.
func (srv *server) dogeBroadcast(r *http.Request) (any, error) {
	var body struct {
		Hex string `json:"hex"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	tx, err := decodeTx(body.Hex)
	if err != nil {
		return nil, badRequest("invalid transaction: %v", err)
	}
	txid, err := srv.doge.send(tx)
	if err != nil {
		return nil, broadcastError(err)
	}
	// Take the payment into the index's view of the mempool now, so the
	// coins it spends stop being offered at once, not on the next pass.
	if srv.dogeIdx != nil {
		if err := srv.dogeIdx.refreshMempool(); err != nil {
			log.Printf("dogecoin index mempool: %v", err)
		}
	}
	return map[string]string{"txid": txid.String()}, nil
}
