package main

import (
	"bytes"
	"net/http"
	"sort"
	"strconv"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// The explorer API: DogecoinVM blocks and transactions, with bridge
// transactions described in plain words, the bridge's activity across both
// chains, and the outputs on Dogecoin that back the peg.

// ioView is one input or output of a transaction.
type ioView struct {
	Address string `json:"address,omitempty"`
	Value   string `json:"value"`
	Note    string `json:"note,omitempty"`
}

// txView is a DogecoinVM transaction for the explorer.
type txView struct {
	TxID          string   `json:"txid"`
	Confirmations int64    `json:"confirmations"`
	BlockHash     string   `json:"blockHash,omitempty"`
	Time          int64    `json:"time,omitempty"`
	Kind          string   `json:"kind"`  // transfer, credit, withdrawal, reserve, reward
	Label         string   `json:"label"` // plain-words description
	Inputs        []ioView `json:"inputs"`
	Outputs       []ioView `json:"outputs"`
	Fee           string   `json:"fee,omitempty"`
	// Links to the other side of a bridge transaction, on Dogecoin.
	DogecoinTxID string `json:"dogecoinTxid,omitempty"`
}

func (srv *server) outputAddress(script []byte) string {
	_, addrs, _, err := txscript.ExtractPkScriptAddrs(script, srv.b.vmParams)
	if err != nil || len(addrs) != 1 {
		return ""
	}
	return addrs[0].EncodeAddress()
}

// describe fills in what a transaction does. A bridge tag alone proves
// nothing, since anyone can write one: a credit must spend the peg reserve,
// which only the signers can do, and a withdrawal must pay into it.
func (srv *server) describe(v *txView, tx *wire.MsgTx, spendsReserve bool) {
	reserve := srv.b.signers.pkScript()
	paysReserve := false
	for _, out := range tx.TxOut {
		paysReserve = paysReserve || bytes.Equal(out.PkScript, reserve)
	}
	snap := srv.current()
	switch {
	case isCoinbase(tx):
		v.Kind, v.Label = "reward", "Block reward: the fees paid in this block"
		for _, out := range tx.TxOut {
			if bytes.Equal(out.PkScript, reserve) {
				v.Kind = "reserve"
				v.Label = "Peg reserve created by consensus. It is locked to the peg signers and only released against DOGE locked on Dogecoin."
			}
		}
	default:
		if deposit, ok := parseRelease(tx); ok && spendsReserve {
			v.Kind = "credit"
			v.Label = "Credit for a deposit on Dogecoin, released from the peg reserve"
			v.DogecoinTxID = deposit.Hash.String()
			return
		}
		if dest, ok := parseDestinationTag(tx, tagPegOut); ok && paysReserve && !spendsReserve {
			addr, _ := dest.address(srv.b.dogeParams)
			v.Kind = "withdrawal"
			v.Label = "Withdrawal to " + addr.EncodeAddress() + " on Dogecoin"
			if snap != nil && snap.state != nil {
				if payment, ok := snap.state.paid[tx.TxHash()]; ok {
					v.DogecoinTxID = payment.String()
					v.Label += ", paid"
				} else {
					v.Label += ", waiting for the bridge"
				}
			}
			return
		}
		if _, _, ok := opReturnData(tx); ok {
			v.Kind, v.Label = "transfer", "Transfer on DogecoinVM with an unverified bridge message"
			return
		}
		v.Kind, v.Label = "transfer", "Transfer on DogecoinVM"
	}
}

// prevOut returns the output op spends, from a cache: outputs never change.
func (srv *server) prevOut(op wire.OutPoint) (*wire.TxOut, bool) {
	srv.cacheMu.Lock()
	out, ok := srv.prevOuts[op]
	srv.cacheMu.Unlock()
	if ok {
		return out, true
	}
	var prevHex string
	if err := srv.vm.rpc.call(&prevHex, "getrawtransaction", op.Hash.String(), 0); err != nil {
		return nil, false
	}
	prev, err := decodeTx(prevHex)
	if err != nil || int(op.Index) >= len(prev.TxOut) {
		return nil, false
	}
	out = prev.TxOut[op.Index]
	srv.cacheMu.Lock()
	if len(srv.prevOuts) > 50_000 {
		srv.prevOuts = map[wire.OutPoint]*wire.TxOut{}
	}
	srv.prevOuts[op] = out
	srv.cacheMu.Unlock()
	return out, true
}

// txDetail fetches and describes a DogecoinVM transaction.
func (srv *server) txDetail(txid string) (*txView, error) {
	var raw struct {
		Hex           string `json:"hex"`
		Confirmations int64  `json:"confirmations"`
		BlockHash     string `json:"blockhash"`
		Time          int64  `json:"time"`
	}
	if err := srv.vm.rpc.call(&raw, "getrawtransaction", txid, 1); err != nil {
		return nil, &apiError{http.StatusNotFound, "no such transaction on DogecoinVM"}
	}
	tx, err := decodeTx(raw.Hex)
	if err != nil {
		return nil, err
	}
	v := &txView{
		TxID: tx.TxHash().String(), Confirmations: raw.Confirmations,
		BlockHash: raw.BlockHash, Time: raw.Time,
		Inputs: []ioView{}, Outputs: []ioView{},
	}

	var in, out int64
	allKnown, spendsReserve := true, false
	if !isCoinbase(tx) {
		for _, txIn := range tx.TxIn {
			io := ioView{Note: "unknown"}
			if prevOut, ok := srv.prevOut(txIn.PreviousOutPoint); ok {
				io = ioView{Address: srv.outputAddress(prevOut.PkScript), Value: formatDoge(prevOut.Value)}
				if bytes.Equal(prevOut.PkScript, srv.b.signers.pkScript()) {
					io.Note = "peg reserve"
					spendsReserve = true
				}
				in += prevOut.Value
			} else {
				allKnown = false
			}
			v.Inputs = append(v.Inputs, io)
		}
	}
	for _, txOut := range tx.TxOut {
		out += txOut.Value
		io := ioView{Address: srv.outputAddress(txOut.PkScript), Value: formatDoge(txOut.Value)}
		switch {
		case bytes.Equal(txOut.PkScript, srv.b.signers.pkScript()):
			io.Note = "peg reserve"
		case txscript.GetScriptClass(txOut.PkScript) == txscript.NullDataTy:
			io.Note = "bridge message"
		}
		v.Outputs = append(v.Outputs, io)
	}
	if !isCoinbase(tx) && allKnown && in >= out {
		v.Fee = formatDoge(in - out)
	}
	srv.describe(v, tx, spendsReserve)
	return v, nil
}

func (srv *server) txHandler(r *http.Request) (any, error) {
	if _, err := chainhash.NewHashFromStr(r.PathValue("txid")); err != nil {
		return nil, badRequest("invalid txid")
	}
	return srv.txDetail(r.PathValue("txid"))
}

type blockView struct {
	Height   int64    `json:"height"`
	Hash     string   `json:"hash"`
	Time     int64    `json:"time"`
	TxCount  int      `json:"txCount"`
	Previous string   `json:"previousHash,omitempty"`
	TxIDs    []string `json:"txids,omitempty"`
}

func (srv *server) block(hash string, withTxs bool) (*blockView, error) {
	var b struct {
		Hash     string   `json:"hash"`
		Height   int64    `json:"height"`
		Time     int64    `json:"time"`
		Tx       []string `json:"tx"`
		Previous string   `json:"previousblockhash"`
	}
	if err := srv.vm.rpc.call(&b, "getblock", hash, 1); err != nil {
		return nil, &apiError{http.StatusNotFound, "no such block on DogecoinVM"}
	}
	v := &blockView{Height: b.Height, Hash: b.Hash, Time: b.Time, TxCount: len(b.Tx), Previous: b.Previous}
	if withTxs {
		v.TxIDs = b.Tx
	}
	return v, nil
}

func (srv *server) blocksHandler(r *http.Request) (any, error) {
	var tip int64
	if err := srv.vm.rpc.call(&tip, "getblockcount"); err != nil {
		return nil, err
	}
	from := tip
	if s := r.URL.Query().Get("before"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return nil, badRequest("invalid before")
		}
		from = min(tip, n-1)
	}
	blocks := []*blockView{}
	for h := from; h >= 0 && len(blocks) < 15; h-- {
		var hash string
		if err := srv.vm.rpc.call(&hash, "getblockhash", h); err != nil {
			return nil, err
		}
		b, err := srv.block(hash, false)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, b)
	}
	return map[string]any{"tip": tip, "blocks": blocks}, nil
}

func (srv *server) blockHandler(r *http.Request) (any, error) {
	id := r.PathValue("id")
	if height, err := strconv.ParseInt(id, 10, 64); err == nil {
		if err := srv.vm.rpc.call(&id, "getblockhash", height); err != nil {
			return nil, &apiError{http.StatusNotFound, "no block at that height"}
		}
	}
	b, err := srv.block(id, true)
	if err != nil {
		return nil, err
	}
	txs := []*txView{}
	total := len(b.TxIDs)
	// Describing a transaction costs an RPC per input; cap the work a
	// single request can cause.
	if len(b.TxIDs) > 50 {
		b.TxIDs = b.TxIDs[:50]
	}
	for _, txid := range b.TxIDs {
		v, err := srv.txDetail(txid)
		if err != nil {
			return nil, err
		}
		txs = append(txs, v)
	}
	return map[string]any{"block": b, "transactions": txs, "txCount": total}, nil
}

// activity is the bridge's moves across both chains, newest first.
func (srv *server) activityHandler(*http.Request) (any, error) {
	snap := srv.current()
	events := []map[string]any{}
	if snap == nil || snap.state == nil {
		return events, nil
	}
	s := snap.state
	vmAddr := func(d destination) string {
		a, _ := d.address(srv.b.vmParams)
		return a.EncodeAddress()
	}
	addDeposit := func(d deposit, status string) {
		e := map[string]any{
			"type": "deposit", "time": d.time, "dogecoinTxid": d.outPoint.Hash.String(),
			"vout": d.outPoint.Index, "amount": formatDoge(d.value),
			"confirmations": d.confirmations, "required": srv.b.confirmationsFor(d.value), "status": status,
		}
		if d.valid {
			e["to"] = vmAddr(d.dest)
		}
		if credit, ok := s.released[d.outPoint]; ok {
			e["status"], e["creditTxid"], e["credited"] = "credited", credit.String(), formatDoge(d.value-srv.b.vmFee)
		}
		if refund, ok := s.refunded[d.outPoint]; ok {
			e["status"], e["refundTxid"] = "refunded", refund.String()
		}
		events = append(events, e)
	}
	for _, d := range s.deposits {
		addDeposit(d, "waiting")
	}
	for _, d := range s.held {
		addDeposit(d, "held")
	}
	for op, refund := range s.refunded {
		events = append(events, map[string]any{
			"type": "deposit", "dogecoinTxid": op.Hash.String(), "vout": op.Index,
			"status": "refunded", "refundTxid": refund.String(),
		})
	}
	for _, p := range s.pegOuts {
		to, _ := p.dest.address(srv.b.dogeParams)
		e := map[string]any{
			"type": "withdrawal", "time": p.time, "dogecoinvmTxid": p.txid.String(),
			"amount": formatDoge(p.value), "pays": formatDoge(p.value - srv.b.dogeFee),
			"to": to.EncodeAddress(), "status": "waiting",
		}
		if payment, ok := s.paid[p.txid]; ok {
			e["status"], e["dogecoinTxid"] = "paid", payment.String()
		}
		events = append(events, e)
	}
	sort.SliceStable(events, func(i, j int) bool {
		ti, _ := events[i]["time"].(int64)
		tj, _ := events[j]["time"].(int64)
		return ti > tj
	})
	if len(events) > 100 {
		events = events[:100]
	}
	return events, nil
}

// reserves lists the outputs on Dogecoin that back the peg, so anyone can
// check them on a public explorer, next to what circulates on DogecoinVM.
func (srv *server) reservesHandler(*http.Request) (any, error) {
	snap := srv.current()
	if snap == nil || snap.state == nil {
		return nil, &apiError{http.StatusServiceUnavailable, "starting up"}
	}
	s := snap.state
	outputs := []map[string]any{}
	for _, u := range s.lockedUTXOs {
		_, addrs, _, _ := txscript.ExtractPkScriptAddrs(u.pkScript, srv.b.dogeParams)
		a := ""
		if len(addrs) == 1 {
			a = addrs[0].EncodeAddress()
		}
		outputs = append(outputs, map[string]any{
			"txid": u.outPoint.Hash.String(), "vout": u.outPoint.Index,
			"amount": formatDoge(u.value), "confirmations": u.confirmations, "address": a,
		})
	}
	pegAddr, _ := srv.b.dogePegAddress()
	reserveAddr, _ := srv.b.vmReserveAddress()
	return map[string]any{
		"lockedOnDogecoin":     formatDoge(s.locked),
		"circulating":          formatDoge(s.reserveCreated - s.reserveUnspent),
		"reserveCreated":       formatDoge(s.reserveCreated),
		"reserveHeld":          formatDoge(s.reserveUnspent),
		"pegAddress":           pegAddr.EncodeAddress(),
		"reserveAddress":       reserveAddr.EncodeAddress(),
		"dogecoinOutputs":      outputs,
		"personalDepositCount": len(s.redeemFor) - 1,
	}, nil
}
