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
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/MetalBlockchain/btcvm/btcd/btcec/v2"
	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg/chainhash"
)

// all: includes files starting with _, such as noble-hashes/_md.js.
//
//go:embed all:web
var webFiles embed.FS

// server is the public web API behind the web wallet. It holds the node RPC
// credentials; browsers get read access, broadcast, deposit-address
// registration and the faucet, and never see a key or password.
type server struct {
	b      *bridge
	vm     *vmChain
	doge   *dogeChain
	faucet *faucet

	mu       sync.RWMutex
	snapshot *snapshot

	registerLimit *rateLimit
}

// snapshot is the bridge state, refreshed in the background so requests do
// not each rescan both chains.
type snapshot struct {
	state      *pegState
	audit      audit
	vmHeight   int64
	dogeHeight int64
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
	_ = srv.vm.rpc.call(&snap.vmHeight, "getblockcount")
	_ = srv.doge.rpc.call(&snap.dogeHeight, "getblockcount")

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
		"vmFee":                formatDoge(srv.b.vmFee),
		"dogeFee":              formatDoge(srv.b.dogeFee),
		"minDeposit":           formatDoge(srv.b.minDeposit),
		"minPegOut":            formatDoge(srv.b.minPegOut),
		"faucet":               srv.faucet.info(),
		"dogecoinvmVersions":   addressVersions(srv.b.vmParams),
		"dogecoinVersions":     addressVersions(srv.b.dogeParams),
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
		"updated":          snap.updated.UTC().Format(time.RFC3339),
	}
	if snap.err != "" {
		out["error"] = snap.err
	} else {
		out["audit"] = formatAudit(snap.audit)
	}
	return out, nil
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
		outs = append(outs, map[string]any{
			"txid": u.outPoint.Hash.String(), "vout": u.outPoint.Index,
			"value": u.value, "script": hex.EncodeToString(u.pkScript),
			"confirmations": u.confirmations,
		})
	}

	script := destinationScript(addr)
	history := []map[string]any{}
	for i := len(txs) - 1; i >= 0 && len(history) < 50; i-- {
		var received int64
		for _, out := range txs[i].tx.TxOut {
			if string(out.PkScript) == string(script) {
				received += out.Value
			}
		}
		history = append(history, map[string]any{
			"txid": txs[i].tx.TxHash().String(), "confirmations": txs[i].confirmations,
			"received": formatDoge(received),
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
	txid, err := srv.vm.send(tx)
	if err != nil {
		return nil, badRequest("rejected: %v", err)
	}
	return map[string]string{"txid": txid.String()}, nil
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
	for _, d := range snap.state.deposits {
		if d.dest != dest {
			continue
		}
		entry := map[string]any{
			"txid": d.outPoint.Hash.String(), "vout": d.outPoint.Index,
			"amount": formatDoge(d.value), "confirmations": d.confirmations,
			"required": srv.b.depositConfirmations,
		}
		if release, ok := snap.state.released[d.outPoint]; ok {
			entry["creditTxid"] = release.String()
			entry["credited"] = formatDoge(d.value - srv.b.vmFee)
		}
		out = append(out, entry)
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
	s.register(flags)
	b := bridgeFlags(flags)
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
	b.connect(&s, signers)
	b.registry = registryFor(*depositsPath, *signersPath)

	srv := &server{
		b:             b,
		vm:            b.vm.(*vmChain),
		doge:          b.doge.(*dogeChain),
		registerLimit: newRateLimit(30, time.Hour),
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
	if err := watchPeg(b, false); err != nil {
		return fmt.Errorf("importing peg addresses into Dogecoin Core: %w", err)
	}

	go func() {
		for {
			srv.refresh()
			time.Sleep(15 * time.Second)
		}
	}()

	static, err := fs.Sub(webFiles, "web")
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/info", handle(srv.info))
	mux.HandleFunc("GET /api/status", handle(srv.status))
	mux.HandleFunc("GET /api/address/{addr}", handle(srv.address))
	mux.HandleFunc("GET /api/deposits/{addr}", handle(srv.deposits))
	mux.HandleFunc("GET /api/pegout/{txid}", handle(srv.pegOut))
	mux.HandleFunc("POST /api/tx", handle(srv.broadcast))
	mux.HandleFunc("POST /api/deposit-address", handle(srv.depositAddress))
	mux.HandleFunc("POST /api/faucet", handle(srv.faucetClaim))

	log.Printf("serving on http://%s", *listen)
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	return server.ListenAndServe()
}
