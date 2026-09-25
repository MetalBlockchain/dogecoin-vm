package main

import (
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// all: includes files starting with _, such as noble-hashes/_md.js.
//
//go:embed all:web
var webFiles embed.FS

// server is the public web API behind the web wallet. It holds the node RPC
// credentials; browsers get read access, broadcast, deposit-address
// registration and the faucet, and never see a key or password.
type server struct {
	b       *bridge
	health  *healthChecker
	chainID string // the DogecoinVM chain's ID on Metal, for links
	vm      *vmChain
	doge    *dogeChain
	faucet  *faucet
	dogeIdx *dogeIndex // Dogecoin balances for the wallet; nil if disabled

	finality *finalityMeter // how long payments take to be final, measured live

	mu       sync.RWMutex
	snapshot *snapshot

	supply atomic.Pointer[dogeSupply]

	cacheMu  sync.Mutex
	prevOuts map[wire.OutPoint]*wire.TxOut

	// heavy bounds concurrent explorer requests, each of which can make
	// many RPC calls.
	heavy chan struct{}

	registerLimit *rateLimit
}

// dogeSync is the part of Dogecoin Core's getblockchaininfo the page shows
// while the node catches up; deposits are not seen until it has.
type dogeSync struct {
	Headers              int64   `json:"headers"`
	VerificationProgress float64 `json:"verificationprogress"`
}

// snapshot is the bridge state, refreshed in the background so requests do
// not each rescan both chains.
type snapshot struct {
	state      *pegState
	audit      audit
	vmHeight   int64
	dogeHeight int64
	dogeSync   dogeSync
	dogeTime   int64 // when the latest Dogecoin block was found, unix seconds
	checks     []check
	updated    time.Time
	err        string
}

func (srv *server) refresh() {
	snap := &snapshot{updated: time.Now()}
	state, err := srv.b.load()
	if err != nil {
		snap.err = err.Error()
	} else {
		snap.state = state
		snap.audit = srv.b.audit(state)
	}
	snap.checks = srv.health.run(state, err)
	_ = srv.vm.rpc.call(&snap.vmHeight, "getblockcount")
	_ = srv.doge.rpc.call(&snap.dogeHeight, "getblockcount")
	_ = srv.doge.rpc.call(&snap.dogeSync, "getblockchaininfo")
	var best string
	if srv.doge.rpc.call(&best, "getbestblockhash") == nil {
		var header struct {
			Time int64 `json:"time"`
		}
		if srv.doge.rpc.call(&header, "getblockheader", best) == nil {
			snap.dogeTime = header.Time
		}
	}

	srv.mu.Lock()
	srv.snapshot = snap
	srv.mu.Unlock()
}

func (srv *server) current() *snapshot {
	srv.mu.RLock()
	defer srv.mu.RUnlock()
	return srv.snapshot
}

type apiError struct {
	status int
	msg    string
}

func (e *apiError) Error() string { return e.msg }

func badRequest(format string, args ...any) error {
	return &apiError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// handle adapts a handler returning a JSON value or an error.
func handle(fn func(r *http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		result, err := fn(r)
		if err != nil {
			status := http.StatusInternalServerError
			var aerr *apiError
			if errors.As(err, &aerr) {
				status = aerr.status
			} else {
				log.Printf("%s %s: %v", r.Method, r.URL.Path, err)
			}
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}
}

func decodeBody(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

func (srv *server) vmAddress(s string) (btcutil.Address, destination, error) {
	addr, err := btcutil.DecodeAddress(s, srv.b.vmParams)
	if err != nil || !addr.IsForNet(srv.b.vmParams) {
		return nil, destination{}, badRequest("not a DogecoinVM %s address: %q", srv.b.vmParams.Name, s)
	}
	dest, err := destinationOf(addr)
	if err != nil {
		return nil, destination{}, badRequest("%v", err)
	}
	return addr, dest, nil
}

func (srv *server) info(*http.Request) (any, error) {
	pegAddr, err := srv.b.dogePegAddress()
	if err != nil {
		return nil, err
	}
	reserveAddr, err := srv.b.vmReserveAddress()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"dogecoinvmNetwork":    srv.b.vmParams.Name,
		"dogecoinNetwork":      srv.b.dogeParams.Name,
		"pegAddress":           pegAddr.EncodeAddress(),
		"reserveAddress":       reserveAddr.EncodeAddress(),
		"signers":              map[string]any{"required": srv.b.signers.Required, "publicKeys": srv.b.signers.PublicKeys},
		"depositConfirmations": srv.b.depositConfirmations,
		"confirmationTiers":    tiersForAPI(srv.b.confirmationTiers),
		"vmFee":                formatDoge(srv.b.vmFee),
		"dogeFee":              formatDoge(srv.b.dogeFee),
		"minDeposit":           formatDoge(srv.b.minDeposit),
		"minPegOut":            formatDoge(srv.b.minPegOut),
		"maxDeposit":           formatDoge(srv.b.maxDeposit),
		"maxCirculating":       formatDoge(srv.b.maxCirculating),
		"faucet":               srv.faucet.info(),
		"dogeWallet":           srv.dogeIdx != nil,
		"dogecoinvmVersions":   addressVersions(srv.b.vmParams),
		"dogecoinVersions":     addressVersions(srv.b.dogeParams),
		"chainID":              srv.chainID,
	}, nil
}

// addressVersions are the base58 version bytes the web wallet needs to
// encode and check addresses and keys for a network.
func addressVersions(p *chaincfg.Params) map[string]byte {
	return map[string]byte{"p2pkh": p.PubKeyHashAddrID, "p2sh": p.ScriptHashAddrID, "wif": p.PrivateKeyID}
}

func formatAudit(a audit) map[string]any {
	return map[string]any{
		"solvent":               a.solvent(),
		"circulating":           formatDoge(a.Circulating),
		"locked":                formatDoge(a.Locked),
		"pendingPegIns":         formatDoge(a.PendingPegIns),
		"pendingPegOuts":        formatDoge(a.PendingPegOuts),
		"surplus":               formatDoge(a.Surplus),
		"unclaimedOnDogecoin":   formatDoge(a.UnclaimedOnDoge),
		"unclaimedOnDogecoinVM": formatDoge(a.UnclaimedOnVM),
	}
}

func (srv *server) status(*http.Request) (any, error) {
	snap := srv.current()
	if snap == nil {
		return nil, &apiError{http.StatusServiceUnavailable, "starting up"}
	}
	out := map[string]any{
		"dogecoinvmHeight": snap.vmHeight,
		"dogecoinHeight":   snap.dogeHeight,
		"dogecoinSync": map[string]any{
			"headers":  snap.dogeSync.Headers,
			"progress": snap.dogeSync.VerificationProgress,
			// Dogecoin Core reports progress just under 1 when caught up.
			"syncing": snap.dogeSync.Headers > 0 && snap.dogeHeight < snap.dogeSync.Headers-6,
			// No headers means Dogecoin Core did not answer.
			"available": snap.dogeSync.Headers > 0,
		},
		"updated": snap.updated.UTC().Format(time.RFC3339),
		// Dogecoin blocks come at random, a minute apart on average; wallets
		// say so when one is slow.
		"dogecoinBlockTime": snap.dogeTime,
	}
	if p := srv.b.paused(); p != nil {
		out["paused"] = p
	}
	if supply := srv.supply.Load(); supply != nil {
		out["dogecoinSupply"] = supply
	}
	if f := srv.finality.summary(); f != nil {
		out["finality"] = f
	}
	if snap.err != "" {
		out["error"] = snap.err
	} else {
		out["audit"] = formatAudit(snap.audit)
	}
	return out, nil
}

// healthHandler reports the health checks: 200 if all pass, 503 if one
// fails, for uptime monitors.
func (srv *server) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	snap := srv.current()
	if snap == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "checks": []check{}})
		return
	}
	// A Dogecoin node catching up, or a deliberate pause, is degraded, not
	// down: uptime monitors should not page for them.
	status := "ok"
	for _, c := range snap.checks {
		if c.OK {
			continue
		}
		if (c.Name == "dogecoin" && strings.HasPrefix(c.Detail, "still syncing")) || c.Name == "pause" {
			if status == "ok" {
				status = "degraded"
			}
			continue
		}
		status = "down"
	}
	if status == "down" {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok": status == "ok", "status": status, "checks": snap.checks, "updated": snap.updated.UTC().Format(time.RFC3339),
	})
}

func (srv *server) address(r *http.Request) (any, error) {
	addr, _, err := srv.vmAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	utxos, err := srv.vm.unspent([]btcutil.Address{addr}, 0)
	if err != nil {
		return nil, err
	}
	txs, err := srv.vm.addressTxs(addr)
	if err != nil {
		return nil, err
	}

	var confirmed, pending int64
	outs := []map[string]any{}
	for _, u := range utxos {
		if u.confirmations > 0 {
			confirmed += u.value
		} else {
			pending += u.value
		}
		// Values are strings so JavaScript never rounds them.
		outs = append(outs, map[string]any{
			"txid": u.outPoint.Hash.String(), "vout": u.outPoint.Index,
			"value": strconv.FormatInt(u.value, 10), "script": hex.EncodeToString(u.pkScript),
			"confirmations": u.confirmations,
		})
	}

	// The address's outputs across its history, so each transaction's net
	// effect counts what it spent from the address, not just what it paid in.
	script := string(destinationScript(addr))
	owned := map[wire.OutPoint]int64{}
	for _, t := range txs {
		hash := t.tx.TxHash()
		for i, out := range t.tx.TxOut {
			if string(out.PkScript) == script {
				owned[wire.OutPoint{Hash: hash, Index: uint32(i)}] = out.Value
			}
		}
	}
	history := []map[string]any{}
	for i := len(txs) - 1; i >= 0 && len(history) < 50; i-- {
		var in, out int64
		for _, txIn := range txs[i].tx.TxIn {
			in += owned[txIn.PreviousOutPoint]
		}
		for _, o := range txs[i].tx.TxOut {
			if string(o.PkScript) == script {
				out += o.Value
			}
		}
		history = append(history, map[string]any{
			"txid": txs[i].tx.TxHash().String(), "confirmations": txs[i].confirmations,
			"net": formatSigned(out - in), "time": txs[i].time,
		})
	}
	return map[string]any{
		"address":   addr.EncodeAddress(),
		"confirmed": formatDoge(confirmed),
		"pending":   formatDoge(pending),
		"utxos":     outs,
		"history":   history,
	}, nil
}

// dogeSupply is all the DOGE in existence, as the bridge's Dogecoin node
// counts it.
type dogeSupply struct {
	Amount string `json:"amount"` // whole DOGE
	Height int64  `json:"height"`
}

// watchSupply reads Dogecoin's supply from the node's UTXO set once an hour,
// once the node has caught up; before that the figure would be for the past.
// Summing the UTXO set takes minutes, so it gets its own long timeout.
func (srv *server) watchSupply() {
	d := srv.doge.rpc
	rpc := newRPCClient(d.url, d.user, d.pass)
	rpc.http.Timeout = 30 * time.Minute
	for {
		if snap := srv.current(); snap != nil && snap.dogeSync.Headers > 0 && snap.dogeHeight >= snap.dogeSync.Headers-6 {
			var info struct {
				Height      int64       `json:"height"`
				TotalAmount json.Number `json:"total_amount"`
			}
			if err := rpc.call(&info, "gettxoutsetinfo"); err != nil {
				log.Printf("dogecoin supply: %v", err)
			} else {
				// Over 10^11 DOGE is more koinu than an int64 holds, so keep
				// whole DOGE as text.
				whole, _, _ := strings.Cut(info.TotalAmount.String(), ".")
				srv.supply.Store(&dogeSupply{Amount: whole, Height: info.Height})
				time.Sleep(time.Hour)
				continue
			}
		}
		time.Sleep(time.Minute)
	}
}

// limited runs fn once a slot among srv.heavy is free, or fails fast if the
// server is saturated.
func (srv *server) limited(fn func(*http.Request) (any, error)) func(*http.Request) (any, error) {
	return func(r *http.Request) (any, error) {
		select {
		case srv.heavy <- struct{}{}:
			defer func() { <-srv.heavy }()
			return fn(r)
		case <-time.After(10 * time.Second):
			return nil, &apiError{http.StatusServiceUnavailable, "busy; try again shortly"}
		}
	}
}

// rawTx returns a DogecoinVM transaction's bytes, so the wallet can check the
// value of each output it spends against the transaction's own ID.
func (srv *server) rawTx(r *http.Request) (any, error) {
	if _, err := chainhash.NewHashFromStr(r.PathValue("txid")); err != nil {
		return nil, badRequest("invalid txid")
	}
	var hexTx string
	if err := srv.vm.rpc.call(&hexTx, "getrawtransaction", r.PathValue("txid"), 0); err != nil {
		return nil, &apiError{http.StatusNotFound, "no such transaction on DogecoinVM"}
	}
	return map[string]string{"hex": hexTx}, nil
}

func formatSigned(koinu int64) string {
	if koinu < 0 {
		return "-" + formatDoge(-koinu)
	}
	return formatDoge(koinu)
}

// securityHeaders sets the headers a wallet page should have: scripts and
// connections only from this origin, no framing, no content sniffing.
func securityHeaders(next http.Handler) http.Handler {
	const csp = "default-src 'self'; script-src 'self'; connect-src 'self'; " +
		"style-src 'self' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; " +
		"img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

func (srv *server) broadcast(r *http.Request) (any, error) {
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
	// The finality meter's clock starts now, before the node sees it.
	srv.finality.received(tx.TxHash().String(), time.Now())
	txid, err := srv.vm.send(tx)
	if err != nil {
		srv.finality.forget(tx.TxHash().String())
		return nil, broadcastError(err)
	}
	return map[string]string{"txid": txid.String()}, nil
}

// broadcastError tells a transaction the node refused (400: it was not sent)
// from a node that didn't answer (502: it may or may not have been sent, so
// the wallet must keep its record and check the chain).
func broadcastError(err error) error {
	var rejected *rpcError
	if errors.As(err, &rejected) {
		return badRequest("rejected by the network: %s", rejected.Message)
	}
	log.Printf("broadcast: %v", err)
	return &apiError{http.StatusBadGateway, "no answer from the node, so the transaction may or may not have been sent; check its ID in the explorer before trying again"}
}

func (srv *server) depositAddress(r *http.Request) (any, error) {
	var body struct {
		Address string `json:"address"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	addr, dest, err := srv.vmAddress(body.Address)
	if err != nil {
		return nil, err
	}
	if !srv.registerLimit.allow(clientIP(r)) {
		return nil, &apiError{http.StatusTooManyRequests, "too many new deposit addresses from this IP; try later"}
	}
	depositAddr, err := registerDeposit(srv.b, dest)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		"depositAddress": depositAddr.EncodeAddress(),
		"creditTo":       addr.EncodeAddress(),
		"redeemScript":   hex.EncodeToString(srv.b.signers.depositRedeemScript(dest)),
	}, nil
}

func (srv *server) deposits(r *http.Request) (any, error) {
	_, dest, err := srv.vmAddress(r.PathValue("addr"))
	if err != nil {
		return nil, err
	}
	snap := srv.current()
	out := []map[string]any{}
	if snap == nil || snap.state == nil {
		return out, nil
	}
	s := snap.state
	add := func(d deposit, status, reason string) {
		entry := map[string]any{
			"txid": d.outPoint.Hash.String(), "vout": d.outPoint.Index,
			"amount": formatDoge(d.value), "confirmations": d.confirmations,
			"required": srv.b.confirmationsFor(d.value), "status": status,
		}
		if reason != "" {
			entry["reason"] = reason
		}
		if release, ok := s.released[d.outPoint]; ok {
			entry["status"] = "credited"
			entry["creditTxid"] = release.String()
			entry["credited"] = formatDoge(d.value - srv.b.vmFee)
		}
		if refund, ok := s.refunded[d.outPoint]; ok {
			entry["status"] = "refunded"
			entry["refundTxid"] = refund.String()
		}
		out = append(out, entry)
	}
	for _, d := range s.deposits {
		if d.dest != dest {
			continue
		}
		status := "confirming"
		if d.confirmations >= srv.b.confirmationsFor(d.value) {
			status = "waiting_for_capacity" // the bridge credits within a poll unless the cap blocks it
			if srv.b.maxCirculating == 0 || s.reserveCreated-s.reserveUnspent+d.value <= srv.b.maxCirculating {
				status = "crediting"
			}
		}
		add(d, status, "")
	}
	// Held and refunded deposits to a personal address still name it.
	for _, d := range s.held {
		if d.dest == dest && d.dest != (destination{}) {
			add(d, "held", srv.b.holdReason(d))
		}
	}
	for _, d := range s.settled {
		if d.dest == dest && d.dest != (destination{}) {
			add(d, "refunded", "")
		}
	}
	return out, nil
}

func (srv *server) pegOut(r *http.Request) (any, error) {
	txid, err := chainhash.NewHashFromStr(r.PathValue("txid"))
	if err != nil {
		return nil, badRequest("invalid txid")
	}
	snap := srv.current()
	if snap == nil || snap.state == nil {
		return map[string]any{"status": "unknown"}, nil
	}
	for _, p := range snap.state.pegOuts {
		if p.txid != *txid {
			continue
		}
		to, _ := p.dest.address(srv.b.dogeParams)
		out := map[string]any{
			"status": "pending", "amount": formatDoge(p.value),
			"pays": formatDoge(p.value - srv.b.dogeFee), "to": to.EncodeAddress(),
		}
		if payment, ok := snap.state.paid[p.txid]; ok {
			out["status"] = "paid"
			out["paymentTxid"] = payment.String()
			// How far the payout is on Dogecoin: 0 while it waits for a block.
			var tx struct {
				Confirmations int64 `json:"confirmations"`
			}
			if err := srv.doge.rpc.call(&tx, "getrawtransaction", payment.String(), true); err == nil {
				out["paymentConfirmations"] = tx.Confirmations
			}
		}
		return out, nil
	}
	return map[string]any{"status": "unknown", "note": "not yet final on DogecoinVM, or not a valid peg-out"}, nil
}

func (srv *server) faucetClaim(r *http.Request) (any, error) {
	if srv.faucet == nil {
		return nil, &apiError{http.StatusNotFound, "no faucet on this network"}
	}
	var body struct {
		Address string `json:"address"`
	}
	if err := decodeBody(r, &body); err != nil {
		return nil, err
	}
	addr, _, err := srv.vmAddress(body.Address)
	if err != nil {
		return nil, err
	}
	return srv.faucet.claim(srv.vm, srv.b.vmParams, addr, clientIP(r))
}

// clientIP is the request's client address. Behind a local reverse proxy it
// is the first X-Forwarded-For entry.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			return strings.TrimSpace(strings.Split(fwd, ",")[0])
		}
	}
	return host
}

// rateLimit allows each key n events per window.
type rateLimit struct {
	n      int
	window time.Duration
	mu     sync.Mutex
	events map[string][]time.Time
}

func newRateLimit(n int, window time.Duration) *rateLimit {
	return &rateLimit{n: n, window: window, events: map[string][]time.Time{}}
}

func (l *rateLimit) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	recent := l.events[key][:0]
	for _, t := range l.events[key] {
		if now.Sub(t) < l.window {
			recent = append(recent, t)
		}
	}
	if len(recent) >= l.n {
		l.events[key] = recent
		return false
	}
	l.events[key] = append(recent, now)
	return true
}

// faucet hands out testnet DOGE on DogecoinVM from a key the operator funds
// by pegging in.
type faucet struct {
	key       *btcec.PrivateKey
	amount    int64
	perAddr   *rateLimit
	perIP     *rateLimit
	sendMutex sync.Mutex
}

func (f *faucet) info() map[string]any {
	if f == nil {
		return map[string]any{"enabled": false}
	}
	return map[string]any{"enabled": true, "amount": formatDoge(f.amount)}
}

func (f *faucet) claim(vm *vmChain, params *chaincfg.Params, to btcutil.Address, ip string) (any, error) {
	if !f.perAddr.allow(to.EncodeAddress()) || !f.perIP.allow(ip) {
		return nil, &apiError{http.StatusTooManyRequests, "faucet limit reached; try again tomorrow"}
	}
	// One at a time, so claims do not race for the same outputs.
	f.sendMutex.Lock()
	defer f.sendMutex.Unlock()
	txid, err := payFromKey(vm, params, f.key, destinationScript(to), f.amount, nil)
	if err != nil {
		return nil, fmt.Errorf("faucet: %w", err)
	}
	return map[string]string{"txid": txid.String(), "amount": formatDoge(f.amount)}, nil
}

func cmdServe(args []string) error {
	var s settings
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	listen := flags.String("listen", "127.0.0.1:8080", "address to serve on")
	signersPath := flags.String("signers", "", "peg signer set file (public keys are enough)")
	depositsPath := flags.String("deposits", "", "deposit address registry (default: deposits.json next to -signers)")
	faucetKey := flags.String("faucet-key", "", "private key (WIF or hex) of the faucet's DogecoinVM address; empty disables the faucet")
	faucetAmount := flags.String("faucet-amount", "100", "DOGE per faucet claim")
	chainID := flags.String("chain-id", "", "the DogecoinVM chain's ID on Metal, shown on the page")
	dogeIndexPath := flags.String("doge-index", "", "directory for the wallet's Dogecoin address index (default: dogeindex next to -signers; \"off\" disables Dogecoin balances)")
	s.register(flags)
	b := bridgeFlags(flags)
	health := &healthChecker{b: b}
	health.register(flags)
	if err := flags.Parse(args); err != nil {
		return err
	}
	if err := s.resolve(); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath}); err != nil {
		return err
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	if err := b.connect(&s, signers); err != nil {
		return err
	}
	b.registry = registryFor(*depositsPath, *signersPath)

	srv := &server{
		b:             b,
		health:        health,
		chainID:       *chainID,
		vm:            b.vm.(*vmChain),
		doge:          b.doge.(*dogeChain),
		registerLimit: newRateLimit(30, time.Hour),
		prevOuts:      map[wire.OutPoint]*wire.TxOut{},
		heavy:         make(chan struct{}, 8),
	}
	if *faucetKey != "" {
		key, err := parseKey(*faucetKey)
		if err != nil {
			return fmt.Errorf("-faucet-key: %w", err)
		}
		amount, err := parseDoge(*faucetAmount)
		if err != nil {
			return fmt.Errorf("-faucet-amount: %w", err)
		}
		srv.faucet = &faucet{
			key: key, amount: amount,
			perAddr: newRateLimit(1, 24*time.Hour),
			perIP:   newRateLimit(3, 24*time.Hour),
		}
		faucetAddr, _ := p2pkhAddress(key, s.vmParams)
		log.Printf("faucet: %s DOGE per claim from %s", formatDoge(amount), faucetAddr.EncodeAddress())
	}
	// Watching the peg addresses waits on Dogecoin Core, which can be slow to
	// answer while it syncs; the site must not wait with it.
	go func() {
		for {
			err := watchPeg(b, false)
			if err == nil {
				return
			}
			log.Printf("importing peg addresses into Dogecoin Core (will retry): %v", err)
			time.Sleep(time.Minute)
		}
	}()

	go func() {
		for {
			srv.refresh()
			time.Sleep(15 * time.Second)
		}
	}()
	go srv.watchSupply()
	if *dogeIndexPath != "off" {
		if *dogeIndexPath == "" {
			*dogeIndexPath = filepath.Join(filepath.Dir(*signersPath), "dogeindex")
		}
		idx, err := openDogeIndex(*dogeIndexPath, srv.doge)
		if err != nil {
			return fmt.Errorf("opening the Dogecoin index: %w", err)
		}
		srv.dogeIdx = idx
		srv.finality = newFinalityMeter(filepath.Join(filepath.Dir(*dogeIndexPath), "finality.json"))
		go idx.run(make(chan struct{}))
	}

	// Go does not know the web app manifest's type, and the page is served
	// with nosniff.
	_ = mime.AddExtensionType(".webmanifest", "application/manifest+json")
	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	// A wallet should always run its current code, so browsers check for a
	// newer file each time rather than reusing a cached one.
	files := http.FileServerFS(static)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	if err := handlePages(mux); err != nil {
		return err
	}
	// New blocks are pushed to open wallets, so they refresh the moment a
	// payment is final. Started after the Dogecoin index, which it reads.
	hub := newEventHub()
	go srv.watchBlocks(hub)
	mux.HandleFunc("GET /api/events", srv.events(hub))
	mux.HandleFunc("GET /api/info", handle(srv.info))
	mux.HandleFunc("GET /api/status", handle(srv.status))
	mux.HandleFunc("GET /api/health", srv.healthHandler)
	mux.HandleFunc("GET /api/blocks", handle(srv.limited(srv.blocksHandler)))
	mux.HandleFunc("GET /api/block/{id}", handle(srv.limited(srv.blockHandler)))
	mux.HandleFunc("GET /api/tx/{txid}", handle(srv.limited(srv.txHandler)))
	mux.HandleFunc("GET /api/activity", handle(srv.activityHandler))
	mux.HandleFunc("GET /api/reserves", handle(srv.reservesHandler))
	mux.HandleFunc("GET /api/address/{addr}", handle(srv.limited(srv.address)))
	mux.HandleFunc("GET /api/deposits/{addr}", handle(srv.deposits))
	mux.HandleFunc("GET /api/pegout/{txid}", handle(srv.pegOut))
	mux.HandleFunc("POST /api/tx", handle(srv.broadcast))
	mux.HandleFunc("GET /api/rawtx/{txid}", handle(srv.rawTx))
	mux.HandleFunc("POST /api/deposit-address", handle(srv.depositAddress))
	mux.HandleFunc("POST /api/faucet", handle(srv.faucetClaim))
	mux.HandleFunc("POST /api/doge/watch", handle(srv.dogeWatch))
	mux.HandleFunc("GET /api/doge/address/{addr}", handle(srv.dogeAddressHandler))
	mux.HandleFunc("POST /api/doge/import", handle(srv.limited(srv.dogeImport)))
	mux.HandleFunc("GET /api/doge/rawtx/{txid}", handle(srv.limited(srv.dogeRawTx)))
	mux.HandleFunc("POST /api/doge/tx", handle(srv.dogeBroadcast))

	log.Printf("serving on http://%s", *listen)
	server := &http.Server{
		Addr: *listen, Handler: securityHeaders(mux),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
	}
	return server.ListenAndServe()
}
