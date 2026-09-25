package main

// Separate signers. The bridge process (the coordinator) holds no keys: it
// builds each transaction and asks the signers for signatures. Each signer
// runs "dogevm signer" with one key, on its own machine, reading both chains
// through its own nodes. It signs a proposal only if, from its own view, the
// action is owed (a confirmed deposit not yet credited, a final peg-out not
// yet paid, an approved refund) and the transaction is exactly the one it
// would build itself for that action from those inputs. So a compromised
// coordinator can delay transfers but cannot move locked DOGE: it would need
// Required signers to be compromised too.

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2/ecdsa"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

const (
	chainDogecoinVM = "dogecoinvm"
	chainDogecoin   = "dogecoin"

	actionRelease = "release" // credit a Dogecoin deposit on DogecoinVM
	actionPayout  = "payout"  // pay a DogecoinVM peg-out on Dogecoin
	actionRefund  = "refund"  // return a held deposit on Dogecoin
)

// action is what a proposed transaction is for.
type action struct {
	Kind    string `json:"kind"`
	Deposit string `json:"deposit,omitempty"` // TXID:VOUT, for release and refund
	PegOut  string `json:"pegOut,omitempty"`  // DogecoinVM txid, for payout
	To      string `json:"to,omitempty"`      // refund destination, hex kind||hash160
}

func refundAction(op wire.OutPoint, dest destination) action {
	return action{Kind: actionRefund, Deposit: op.String(), To: encodeDest(dest)}
}

// key identifies the action in a signer's log. Derive it from the parsed
// value, rather than the coordinator's spelling, so equivalent encodings
// (for example TXID:0 and TXID:00) cannot create separate log entries.
func (a action) key() (string, error) {
	switch a.Kind {
	case actionRelease, actionRefund:
		op, err := parseOutPoint(a.Deposit)
		if err != nil {
			return "", err
		}
		return a.Kind + ":" + op.String(), nil
	case actionPayout:
		txid, err := chainhash.NewHashFromStr(a.PegOut)
		if err != nil {
			return "", err
		}
		return a.Kind + ":" + txid.String(), nil
	default:
		return "", fmt.Errorf("unknown action %q", a.Kind)
	}
}

// proposal is a transaction the signers are asked to sign.
type proposal struct {
	Chain  string
	Action action
	// Register lists the personal deposit destinations the transaction
	// involves, so a signer that has not seen them yet starts watching.
	Register []destination

	tx      *wire.MsgTx
	redeems [][]byte
}

type signRequest struct {
	Chain    string   `json:"chain"`
	Action   action   `json:"action"`
	Tx       string   `json:"tx"` // unsigned
	Register []string `json:"register,omitempty"`
}

type signResponse struct {
	PublicKey  string   `json:"publicKey"`
	Signatures []string `json:"signatures"` // one per input, DER with sighash byte
}

func encodeDest(d destination) string {
	return hex.EncodeToString(append([]byte{d.kind}, d.hash[:]...))
}

func parseDest(s string) (destination, error) {
	raw, err := hex.DecodeString(s)
	if err != nil {
		return destination{}, err
	}
	return decodeDestination(raw)
}

// --- coordinator --------------------------------------------------------------

// authorize signs p.tx with Required signatures: from keys this process
// holds, if any, then from remote signers.
func (b *bridge) authorize(p *proposal) error {
	sigs := map[int][][]byte{}
	for i, pub := range b.signers.pubKeys {
		for _, key := range b.signers.privKeys {
			if key.PubKey().IsEqual(pub) {
				s, err := signInputs(p.tx, p.redeems, key)
				if err != nil {
					return err
				}
				sigs[i] = s
			}
		}
	}
	var failures []string
	for _, r := range b.cosigners {
		if len(sigs) >= b.signers.Required {
			break
		}
		index, s, err := r.sign(p, b.signers)
		if err == nil {
			if _, have := sigs[index]; have {
				continue
			}
			err = b.signers.verifyInputs(p.tx, p.redeems, index, s)
		}
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", r.URL, err))
			continue
		}
		sigs[index] = s
	}
	if len(sigs) < b.signers.Required {
		return fmt.Errorf("%d of %d signatures: %s", len(sigs), b.signers.Required, strings.Join(failures, "; "))
	}
	return b.signers.assemble(p.tx, p.redeems, sigs)
}

// remoteSigner is a "dogevm signer" the coordinator asks for signatures.
type remoteSigner struct {
	URL   string `json:"url"`
	Token string `json:"token,omitempty"` // optional, for signers run with -token-file

	auth *btcec.PrivateKey // the coordinator key, if the set names one
}

// Requests to signers are signed with the coordinator key, over the method,
// path, time and body. A signer accepts them for requestSkew either side of
// its clock; a replay in that window is harmless, as signing is idempotent.
const requestSkew = 5 * time.Minute

func requestDigest(method, path string, unix int64, body []byte) []byte {
	bodySum := sha256.Sum256(body)
	msg := fmt.Sprintf("dogevm coordinator request v1\n%s\n%s\n%d\n%x", method, path, unix, bodySum)
	sum := sha256.Sum256([]byte(msg))
	return sum[:]
}

// signerClient is shared by every remote signer; http.Client is safe for
// concurrent use.
var signerClient = &http.Client{Timeout: 2 * time.Minute}

// readCosigners reads a JSON list of {"url", "token"}. The file holds
// tokens, so it lives with the deployment, never in the repository.
func readCosigners(path string) ([]*remoteSigner, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list []*remoteSigner
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, r := range list {
		if r.URL == "" {
			return nil, fmt.Errorf("%s: a signer has no url", path)
		}
		r.URL = strings.TrimRight(r.URL, "/")
	}
	return list, nil
}

func (r *remoteSigner) post(path string, body, result any) error {
	raw, _ := json.Marshal(body)
	return r.do(http.MethodPost, path, raw, result)
}

// status fetches the signer's public key and view of the peg.
func (r *remoteSigner) status(result any) error {
	return r.do(http.MethodGet, "/v1/status", nil, result)
}

func (r *remoteSigner) do(method, path string, body []byte, result any) error {
	req, err := http.NewRequest(method, r.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if r.auth != nil {
		now := time.Now().Unix()
		sig := ecdsa.Sign(r.auth, requestDigest(method, path, now, body))
		req.Header.Set("X-Dogevm-Time", strconv.FormatInt(now, 10))
		req.Header.Set("X-Dogevm-Signature", hex.EncodeToString(sig.Serialize()))
	}
	if r.Token != "" {
		req.Header.Set("Authorization", "Bearer "+r.Token)
	}
	resp, err := signerClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if result == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(result)
}

// sign asks the signer to sign p, returning its position in set and its
// signature for each input.
func (r *remoteSigner) sign(p *proposal, set *signerSet) (int, [][]byte, error) {
	req := signRequest{Chain: p.Chain, Action: p.Action, Tx: encodeTx(p.tx)}
	for _, d := range p.Register {
		req.Register = append(req.Register, encodeDest(d))
	}
	var resp signResponse
	if err := r.post("/v1/sign", req, &resp); err != nil {
		return 0, nil, err
	}
	rawPub, err := hex.DecodeString(resp.PublicKey)
	if err != nil {
		return 0, nil, err
	}
	pub, err := btcec.ParsePubKey(rawPub)
	if err != nil {
		return 0, nil, err
	}
	index := set.indexOf(pub)
	if index < 0 {
		return 0, nil, errors.New("signed with a key that is not in the signer set")
	}
	sigs := make([][]byte, len(resp.Signatures))
	for i, s := range resp.Signatures {
		if sigs[i], err = hex.DecodeString(s); err != nil {
			return 0, nil, err
		}
	}
	return index, sigs, nil
}

// register tells the signer about a new personal deposit address, so its
// Dogecoin node watches it before anything is sent there.
func (r *remoteSigner) register(d destination) error {
	return r.post("/v1/register", map[string]string{"destination": encodeDest(d)}, nil)
}

// --- signer --------------------------------------------------------------------

// cosigner is the "dogevm signer" service: one key, its own view of both
// chains (b, whose signer set holds public keys only), and a log of what it
// has signed.
type cosigner struct {
	b     *bridge
	key   *btcec.PrivateKey
	token string
	log   *signingLog

	// refundApprovals is a file of "TXID:VOUT DOGECOIN-ADDRESS" lines: the
	// refunds this signer's operator has approved. Empty: none.
	refundApprovals string
	// maxDaily caps the DOGE, in koinu, this signer approves moving in any
	// 24 hours. Zero: no cap.
	maxDaily int64
	// open accepts unauthenticated requests: only on loopback, where the
	// coordinator runs on the same machine.
	open bool

	mu sync.Mutex // one proposal at a time
}

func (c *cosigner) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sign", c.authed(c.handleSign))
	mux.HandleFunc("POST /v1/register", c.authed(c.handleRegister))
	mux.HandleFunc("GET /v1/status", c.authed(c.handleStatus))
	return mux
}

// authenticate checks a request carries the token, if this signer has one,
// and the coordinator's signature, if the signer set names a coordinator
// key. It returns the body.
func (c *cosigner) authenticate(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(c.token)) != 1 {
			return nil, errors.New("unauthorized")
		}
	}
	if coord := c.b.signers.coordKey; coord != nil {
		unix, err := strconv.ParseInt(r.Header.Get("X-Dogevm-Time"), 10, 64)
		if err != nil {
			return nil, errors.New("unauthorized: not signed by the coordinator")
		}
		if d := time.Since(time.Unix(unix, 0)); d > requestSkew || d < -requestSkew {
			return nil, errors.New("unauthorized: request time is too far from this signer's clock")
		}
		raw, err := hex.DecodeString(r.Header.Get("X-Dogevm-Signature"))
		if err != nil {
			return nil, errors.New("unauthorized: not signed by the coordinator")
		}
		sig, err := ecdsa.ParseDERSignature(raw)
		if err != nil || !sig.Verify(requestDigest(r.Method, r.URL.Path, unix, body), coord) {
			return nil, errors.New("unauthorized: not signed by the coordinator")
		}
	}
	if c.token == "" && c.b.signers.coordKey == nil && !c.open {
		return nil, errors.New("unauthorized: this signer has neither a token nor a coordinator key")
	}
	return body, nil
}

func (c *cosigner) authed(fn func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, err := c.authenticate(r)
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		c.mu.Lock()
		result, err := fn(r)
		c.mu.Unlock()
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}
		_ = json.NewEncoder(w).Encode(result)
	}
}

func (c *cosigner) handleRegister(r *http.Request) (any, error) {
	var body struct {
		Destination string `json:"destination"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	d, err := parseDest(body.Destination)
	if err != nil {
		return nil, err
	}
	addr, err := registerDeposit(c.b, d)
	if err != nil {
		return nil, err
	}
	return map[string]string{"depositAddress": addr.EncodeAddress()}, nil
}

func (c *cosigner) handleStatus(*http.Request) (any, error) {
	s, err := c.b.load()
	if err != nil {
		return nil, err
	}
	a := c.b.audit(s)
	return map[string]any{
		"publicKey":   hex.EncodeToString(c.key.PubKey().SerializeCompressed()),
		"solvent":     a.solvent(),
		"locked":      formatDoge(a.Locked),
		"circulating": formatDoge(a.Circulating),
		"signedToday": formatDoge(c.log.volumeSince(time.Now().Add(-24 * time.Hour))),
	}, nil
}

func (c *cosigner) handleSign(r *http.Request) (any, error) {
	var req signRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, err
	}
	key, err := req.Action.key()
	if err != nil {
		c.b.logf("refused %s: %v", req.Action.Kind, err)
		return nil, err
	}
	tx, redeems, value, err := c.check(req)
	if err != nil {
		c.b.logf("refused %s: %v", key, err)
		return nil, err
	}
	sigs, err := signInputs(tx, redeems, c.key)
	if err != nil {
		return nil, err
	}
	// Record before answering: if the log cannot be written, sign nothing.
	if err := c.log.record(key, tx, value); err != nil {
		return nil, fmt.Errorf("writing the signing log: %w", err)
	}
	c.b.logf("signed %s in %v", key, tx.TxHash())
	resp := signResponse{PublicKey: hex.EncodeToString(c.key.PubKey().SerializeCompressed())}
	for _, s := range sigs {
		resp.Signatures = append(resp.Signatures, hex.EncodeToString(s))
	}
	return resp, nil
}

// check decides whether to sign req, from this signer's own view of both
// chains. It returns the transaction, the redeem script of each input, and
// the DOGE the action moves.
func (c *cosigner) check(req signRequest) (*wire.MsgTx, [][]byte, int64, error) {
	b := c.b
	if p := b.paused(); p != nil {
		return nil, nil, 0, fmt.Errorf("this signer is paused: %s", p.Reason)
	}
	tx, err := decodeTx(req.Tx)
	if err != nil {
		return nil, nil, 0, err
	}
	seen := map[wire.OutPoint]bool{}
	for _, in := range tx.TxIn {
		if len(in.SignatureScript) != 0 || len(in.Witness) != 0 {
			return nil, nil, 0, errors.New("proposal inputs must be unsigned")
		}
		if seen[in.PreviousOutPoint] {
			return nil, nil, 0, errors.New("proposal spends an output twice")
		}
		seen[in.PreviousOutPoint] = true
	}
	for _, h := range req.Register {
		d, err := parseDest(h)
		if err != nil {
			return nil, nil, 0, err
		}
		if _, err := registerDeposit(b, d); err != nil {
			return nil, nil, 0, err
		}
	}

	s, err := b.load()
	if err != nil {
		return nil, nil, 0, err
	}
	if s, err = c.catchUp(s, req, tx); err != nil {
		return nil, nil, 0, err
	}
	if a := b.audit(s); !a.solvent() {
		return nil, nil, 0, fmt.Errorf("%w (%+v)", errInsolvent, a)
	}

	// pick returns the proposal's inputs from outputs this signer knows the
	// peg holds, with their value as this signer sees it.
	pick := func(available []utxo) ([]utxo, int64, error) {
		byOutPoint := map[wire.OutPoint]utxo{}
		for _, u := range available {
			byOutPoint[u.outPoint] = u
		}
		var inputs []utxo
		var total int64
		for _, in := range tx.TxIn {
			u, ok := byOutPoint[in.PreviousOutPoint]
			if !ok {
				return nil, 0, fmt.Errorf("input %v is not a confirmed peg output in this signer's view", in.PreviousOutPoint)
			}
			inputs = append(inputs, u)
			total += u.value
		}
		return inputs, total, nil
	}

	var want *wire.MsgTx
	var redeems [][]byte
	var value int64
	unspent := map[wire.OutPoint]bool{}
	switch req.Action.Kind {
	case actionRelease:
		if req.Chain != chainDogecoinVM {
			return nil, nil, 0, errors.New("a release is a DogecoinVM transaction")
		}
		op, err := parseOutPoint(req.Action.Deposit)
		if err != nil {
			return nil, nil, 0, err
		}
		if txid, done := s.released[op]; done {
			return nil, nil, 0, fmt.Errorf("deposit %v was already credited in %v", op, txid)
		}
		d, ok := findDeposit(s.deposits, op)
		if !ok {
			return nil, nil, 0, fmt.Errorf("no creditable deposit %v in this signer's view", op)
		}
		if need := b.confirmationsFor(d.value); d.confirmations < need {
			return nil, nil, 0, fmt.Errorf("deposit %v has %d of %d confirmations", op, d.confirmations, need)
		}
		if b.maxCirculating > 0 && s.reserveCreated-s.reserveUnspent+d.value > b.maxCirculating {
			return nil, nil, 0, fmt.Errorf("crediting deposit %v would exceed the circulating cap", op)
		}
		inputs, total, err := pick(s.reserveUTXOs)
		if err != nil {
			return nil, nil, 0, err
		}
		if total < d.value {
			return nil, nil, 0, errors.New("inputs do not cover the deposit")
		}
		want = b.buildRelease(inputs, total, d)
		for range inputs {
			redeems = append(redeems, b.signers.redeemScript)
		}
		value = d.value
		reserveAddr, err := b.vmReserveAddress()
		if err != nil {
			return nil, nil, 0, err
		}
		all, err := b.vm.unspent([]btcutil.Address{reserveAddr}, 0)
		if err != nil {
			return nil, nil, 0, err
		}
		for _, u := range all {
			unspent[u.outPoint] = true
		}

	case actionPayout, actionRefund:
		if req.Chain != chainDogecoin {
			return nil, nil, 0, fmt.Errorf("a %s is a Dogecoin transaction", req.Action.Kind)
		}
		var dest destination
		var data []byte
		if req.Action.Kind == actionPayout {
			txid, err := chainhash.NewHashFromStr(req.Action.PegOut)
			if err != nil {
				return nil, nil, 0, err
			}
			if paid, done := s.paid[*txid]; done {
				return nil, nil, 0, fmt.Errorf("peg-out %v was already paid in %v", txid, paid)
			}
			p, ok := findPegOut(s.pegOuts, *txid)
			if !ok {
				return nil, nil, 0, fmt.Errorf("no final peg-out %v in this signer's view", txid)
			}
			value, dest, data = p.value, p.dest, encodePayment(p.txid)
		} else {
			op, err := parseOutPoint(req.Action.Deposit)
			if err != nil {
				return nil, nil, 0, err
			}
			if txid, done := s.refunded[op]; done {
				return nil, nil, 0, fmt.Errorf("deposit %v was already refunded in %v", op, txid)
			}
			// Signers only refund deposits the bridge will never credit.
			d, ok := findDeposit(s.held, op)
			if !ok {
				return nil, nil, 0, fmt.Errorf("deposit %v is not held for a refund in this signer's view", op)
			}
			if dest, err = parseDest(req.Action.To); err != nil {
				return nil, nil, 0, err
			}
			if err := c.refundApproved(op, dest); err != nil {
				return nil, nil, 0, err
			}
			value, data = d.value, encodeRefund(op)
		}
		if value <= b.dogeFee {
			return nil, nil, 0, errors.New("the amount does not cover the Dogecoin fee")
		}
		var confirmed []utxo
		for _, u := range s.lockedUTXOs {
			unspent[u.outPoint] = true
			if u.confirmations > 0 {
				confirmed = append(confirmed, u)
			}
		}
		inputs, total, err := pick(confirmed)
		if err != nil {
			return nil, nil, 0, err
		}
		if total < value {
			return nil, nil, 0, errors.New("inputs do not cover the payment")
		}
		want = b.buildPayout(inputs, total, value, dest, data)
		for _, u := range inputs {
			redeem := s.redeemFor[string(u.pkScript)]
			if redeem == nil {
				return nil, nil, 0, fmt.Errorf("no redeem script for %v", u.outPoint)
			}
			redeems = append(redeems, redeem)
		}

	default:
		return nil, nil, 0, fmt.Errorf("unknown action %q", req.Action.Kind)
	}

	if encodeTx(want) != encodeTx(tx) {
		return nil, nil, 0, errors.New("the proposal is not the transaction this signer would build for that action")
	}
	key, err := req.Action.key()
	if err != nil {
		return nil, nil, 0, err
	}
	if err := c.log.permit(key, tx, unspent); err != nil {
		return nil, nil, 0, err
	}
	if c.maxDaily > 0 && !c.log.has(key) &&
		c.log.volumeSince(time.Now().Add(-24*time.Hour))+value > c.maxDaily {
		return nil, nil, 0, fmt.Errorf("signing would exceed this signer's %s DOGE daily limit", formatDoge(c.maxDaily))
	}
	return tx, redeems, value, nil
}

// catchUp handles a signer that started watching a deposit address after a
// payment to it: its wallet has not seen the deposit, or an output the
// proposal spends. It has its own Dogecoin node prove each such transaction is
// in the chain and adds it to the wallet, then reloads. Nothing is taken from
// the coordinator: a transaction that is not in this node's chain fails.
func (c *cosigner) catchUp(s *pegState, req signRequest, tx *wire.MsgTx) (*pegState, error) {
	dc, ok := c.b.doge.(*dogeChain)
	if !ok {
		return s, nil
	}
	var missing []chainhash.Hash
	switch req.Action.Kind {
	case actionRelease, actionRefund:
		op, err := parseOutPoint(req.Action.Deposit)
		if err != nil {
			return nil, err
		}
		_, credit := findDeposit(s.deposits, op)
		_, held := findDeposit(s.held, op)
		_, released := s.released[op]
		if !credit && !held && !released {
			missing = append(missing, op.Hash)
		}
	}
	if req.Chain == chainDogecoin {
		known := map[wire.OutPoint]bool{}
		for _, u := range s.lockedUTXOs {
			known[u.outPoint] = true
		}
		for _, in := range tx.TxIn {
			if !known[in.PreviousOutPoint] {
				missing = append(missing, in.PreviousOutPoint.Hash)
			}
		}
	}
	if len(missing) == 0 {
		return s, nil
	}
	for _, txid := range missing {
		if err := dc.importTx(txid); err != nil {
			c.b.logf("could not import %v from this signer's Dogecoin node: %v", txid, err)
		}
	}
	return c.b.load()
}

// refundApproved checks the operator listed this refund in the approvals
// file.
func (c *cosigner) refundApproved(op wire.OutPoint, dest destination) error {
	if c.refundApprovals == "" {
		return errors.New("this signer approves no refunds (no -refund-approvals file)")
	}
	raw, err := os.ReadFile(c.refundApprovals)
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || strings.HasPrefix(f[0], "#") || f[0] != op.String() {
			continue
		}
		addr, err := btcutil.DecodeAddress(f[1], c.b.dogeParams)
		if err != nil {
			return fmt.Errorf("approval for %v: %w", op, err)
		}
		approved, err := destinationOf(addr)
		if err != nil {
			return err
		}
		if approved == dest {
			return nil
		}
		return fmt.Errorf("the refund of %v is approved to a different address", op)
	}
	return fmt.Errorf("the refund of %v is not in this signer's approvals", op)
}

func findDeposit(list []deposit, op wire.OutPoint) (deposit, bool) {
	for _, d := range list {
		if d.outPoint == op {
			return d, true
		}
	}
	return deposit{}, false
}

func findPegOut(list []pegOut, txid chainhash.Hash) (pegOut, bool) {
	for _, p := range list {
		if p.txid == txid {
			return p, true
		}
	}
	return pegOut{}, false
}

// --- signing log -------------------------------------------------------------

// signingLog records every transaction a signer has signed, by action. It
// stops the signer signing two transactions for one action that could both
// confirm: a proposal for an action already signed must spend one of the
// same outputs as each earlier transaction still able to confirm, so at most
// one of them can. The log must survive restarts; losing it only weakens
// that check to what the chains show.
type signingLog struct {
	path    string
	Actions map[string]*loggedAction `json:"actions"`
}

type loggedAction struct {
	Value int64      `json:"value"` // koinu the action moves
	First int64      `json:"first"` // unix seconds of the first signature
	Txs   []loggedTx `json:"txs"`
}

type loggedTx struct {
	Txid   string   `json:"txid"`
	Inputs []string `json:"inputs"`
}

func openSigningLog(path string) (*signingLog, error) {
	l := &signingLog{path: path, Actions: map[string]*loggedAction{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return l, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, l); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if l.Actions == nil {
		l.Actions = map[string]*loggedAction{}
	}
	if err := l.normalizeKeys(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return l, nil
}

// normalizeKeys upgrades logs written before action keys were canonical. It
// also merges aliases if a coordinator previously used more than one spelling
// for the same action, preserving every transaction the signer signed.
func (l *signingLog) normalizeKeys() error {
	normalized := make(map[string]*loggedAction, len(l.Actions))
	for rawKey, logged := range l.Actions {
		if logged == nil {
			return fmt.Errorf("signing-log action %q is null", rawKey)
		}
		kind, value, ok := strings.Cut(rawKey, ":")
		if !ok {
			return fmt.Errorf("invalid signing-log action key %q", rawKey)
		}
		a := action{Kind: kind}
		switch kind {
		case actionRelease, actionRefund:
			a.Deposit = value
		case actionPayout:
			a.PegOut = value
		default:
			return fmt.Errorf("invalid signing-log action key %q", rawKey)
		}
		key, err := a.key()
		if err != nil {
			return fmt.Errorf("invalid signing-log action key %q: %w", rawKey, err)
		}
		if existing := normalized[key]; existing != nil {
			if existing.Value != logged.Value {
				return fmt.Errorf("signing-log aliases for %q have different values", key)
			}
			if logged.First < existing.First {
				existing.First = logged.First
			}
			existing.Txs = append(existing.Txs, logged.Txs...)
			continue
		}
		normalized[key] = logged
	}
	l.Actions = normalized
	return nil
}

func (l *signingLog) has(key string) bool { return l.Actions[key] != nil }

// permit checks tx may be signed for the action key. unspent holds the peg
// outputs still unspent, confirmed or not: an earlier transaction spending
// one that is gone can no longer confirm.
func (l *signingLog) permit(key string, tx *wire.MsgTx, unspent map[wire.OutPoint]bool) error {
	a := l.Actions[key]
	if a == nil {
		return nil
	}
	spends := map[string]bool{}
	for _, in := range tx.TxIn {
		spends[in.PreviousOutPoint.String()] = true
	}
	for _, prev := range a.Txs {
		live, conflicts := true, false
		for _, in := range prev.Inputs {
			op, err := parseOutPoint(in)
			if err != nil || !unspent[op] {
				live = false
			}
			conflicts = conflicts || spends[in]
		}
		if live && !conflicts {
			return fmt.Errorf("already signed %s for %s, and this proposal could confirm alongside it", prev.Txid, key)
		}
	}
	return nil
}

// record adds tx to the log and writes it out before returning.
func (l *signingLog) record(key string, tx *wire.MsgTx, value int64) error {
	a := l.Actions[key]
	if a == nil {
		a = &loggedAction{Value: value, First: time.Now().Unix()}
		l.Actions[key] = a
	}
	entry := loggedTx{Txid: tx.TxHash().String()}
	for _, in := range tx.TxIn {
		entry.Inputs = append(entry.Inputs, in.PreviousOutPoint.String())
	}
	a.Txs = append(a.Txs, entry)

	raw, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(l.path), ".signing-log-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), l.path)
}

// volumeSince is the DOGE moved by actions first signed since t.
func (l *signingLog) volumeSince(t time.Time) int64 {
	var total int64
	for _, a := range l.Actions {
		if a.First >= t.Unix() {
			total += a.Value
		}
	}
	return total
}

// --- commands ------------------------------------------------------------------

// cmdSignerKey makes the key for one separate signer. Its operator runs it
// on the signer's own machine and shares only the public key.
func cmdSignerKey(args []string) error {
	fs := flag.NewFlagSet("signer-key", flag.ExitOnError)
	out := fs.String("out", "", "file to write the private key to (hex, mode 0600; never commit it)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := required(map[string]string{"out": *out}); err != nil {
		return err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		return err
	}
	key, _ := btcec.PrivKeyFromBytes(secret[:])
	f, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(f, hex.EncodeToString(secret[:])); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	printJSON(map[string]string{"keyFile": *out, "publicKey": hex.EncodeToString(key.PubKey().SerializeCompressed())})
	return nil
}

// readKeyFile reads a signer's hex private key, refusing a file other users
// can read.
func readKeyFile(path string) (*btcec.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by other users; chmod 600 it", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	secret, err := hex.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(secret) != 32 {
		return nil, fmt.Errorf("%s must hold a 32-byte hex private key", path)
	}
	key, _ := btcec.PrivKeyFromBytes(secret)
	return key, nil
}

func cmdSigner(args []string) error {
	var s settings
	fs := flag.NewFlagSet("signer", flag.ExitOnError)
	signersPath := fs.String("signers", "", "peg signer set file, with public keys only")
	keyFile := fs.String("key-file", "", "this signer's private key (from dogevm signer-key)")
	depositsPath := fs.String("deposits", "", "this signer's deposit address registry (default: deposits.json next to -signers)")
	logPath := fs.String("log", "", "signing log (default: signing-log.json next to -key-file); back it up")
	listen := fs.String("listen", "127.0.0.1:9700", "address to serve the signing API on")
	tokenFile := fs.String("token-file", "", "file holding the token the coordinator must present")
	approvals := fs.String("refund-approvals", "", `file of approved refunds, "TXID:VOUT DOGECOIN-ADDRESS" per line`)
	maxDaily := fs.Int64("max-daily", 0, "most DOGE, in koinu, this signer approves moving in 24 hours (0: no limit)")
	rescan := fs.Bool("rescan", false, "rescan Dogecoin for past payments to the peg and registered deposit addresses")
	s.register(fs)
	b := bridgeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := s.resolve(); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath, "key-file": *keyFile}); err != nil {
		return err
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	if len(signers.PrivateKeys) > 0 {
		return fmt.Errorf("%s holds private keys; a separate signer's set must hold public keys only", *signersPath)
	}
	if b.cosignersPath != "" {
		return errors.New("a signer does not take -cosigners")
	}
	key, err := readKeyFile(*keyFile)
	if err != nil {
		return err
	}
	if signers.indexOf(key.PubKey()) < 0 {
		return errors.New("this key is not in the signer set")
	}
	if err := b.connect(&s, signers); err != nil {
		return err
	}
	b.registry = registryFor(*depositsPath, *signersPath)
	if err := watchPeg(b, *rescan); err != nil {
		return fmt.Errorf("importing the peg addresses into Dogecoin Core: %w", err)
	}
	if *logPath == "" {
		*logPath = filepath.Join(filepath.Dir(*keyFile), "signing-log.json")
	}
	log, err := openSigningLog(*logPath)
	if err != nil {
		return err
	}
	c := &cosigner{b: b, key: key, log: log, refundApprovals: *approvals, maxDaily: *maxDaily}
	if *tokenFile != "" {
		raw, err := os.ReadFile(*tokenFile)
		if err != nil {
			return err
		}
		c.token = strings.TrimSpace(string(raw))
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil {
		return err
	}
	loopback := false
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		loopback = true
	}
	switch {
	case c.token == "" && signers.coordKey == nil && !loopback:
		return errors.New("listening beyond this machine needs a coordinator key in the signer set, or -token-file")
	case c.token == "" && signers.coordKey == nil:
		c.open = true
	}
	b.logf("signer %s listening on %s", hex.EncodeToString(key.PubKey().SerializeCompressed()), *listen)
	srv := &http.Server{
		Addr:              *listen,
		Handler:           c.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
	}
	return srv.ListenAndServe()
}
