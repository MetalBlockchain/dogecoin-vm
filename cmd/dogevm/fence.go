package main

// Fencing against a stale signing log. The log is what stops a signer
// signing two transactions for one action that could both confirm, so a
// signer must never run on a log older than what it has signed: one restored
// from a backup, copied to a clone, or lost and started empty. Its own chain
// nodes are the witness outside the log: every transaction it signed that
// reached either chain carries its signature. A missing log, or one that
// lacks a transaction the chains show it signed, quarantines the signer: it
// signs nothing until its operator restores the log and removes the marker.
//
// This catches a stale log once anything signed after it reaches a chain. A
// signature the coordinator holds back is not visible to it; the anchor
// rule (every transaction on a chain spends the tip of its line, lineage.go)
// keeps such a transaction from confirming alongside another.

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2/ecdsa"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

var errQuarantined = errors.New("this signer is quarantined")

// unsignedHash is a transaction's txid with its signatures stripped: what
// the signing log records, whatever signatures the coordinator assembled.
func unsignedHash(tx *wire.MsgTx) chainhash.Hash {
	c := tx.Copy()
	for _, in := range c.TxIn {
		in.SignatureScript = nil
		in.Witness = nil
	}
	return c.TxHash()
}

// signedBy reports whether any input of tx carries a signature by pub over
// the peg's redeem script or a personal deposit address's.
func signedBy(tx *wire.MsgTx, set *signerSet, pub *btcec.PublicKey) bool {
	if set.indexOf(pub) < 0 {
		return false
	}
	for i, in := range tx.TxIn {
		pushes, err := txscript.PushedData(in.SignatureScript)
		if err != nil || len(pushes) < 2 {
			continue
		}
		redeem := pushes[len(pushes)-1]
		if !set.pegRedeem(redeem) {
			continue
		}
		hash, err := txscript.CalcSignatureHash(redeem, txscript.SigHashAll, tx, i)
		if err != nil {
			continue
		}
		for _, item := range pushes[:len(pushes)-1] {
			if len(item) < 2 || txscript.SigHashType(item[len(item)-1]) != txscript.SigHashAll {
				continue
			}
			sig, err := ecdsa.ParseDERSignature(item[:len(item)-1])
			if err == nil && sig.Verify(hash, pub) {
				return true
			}
		}
	}
	return false
}

// pegRedeem reports whether script is the peg's redeem script, or a
// personal deposit address's.
func (s *signerSet) pegRedeem(script []byte) bool {
	if bytes.Equal(script, s.redeemScript) {
		return true
	}
	n := len(script) - len(s.redeemScript)
	return n == 23 && script[0] == txscript.OP_DATA_21 && script[22] == txscript.OP_DROP &&
		bytes.Equal(script[23:], s.redeemScript)
}

// unlogged returns the transactions on either chain, as s read them, that
// carry this signer's signature but are not in its signing log. Checked
// transactions are remembered: their bytes, and so their signatures, never
// change.
func (c *cosigner) unlogged(s *pegState) []string {
	all := append(append([]chainTx{}, s.vmTxs...), s.dogeTxs...)
	logged := c.log.txids()
	if c.mine == nil {
		c.mine = map[chainhash.Hash]bool{}
	}
	pub := c.key.PubKey()
	var missing []string
	for _, t := range all {
		txid := t.tx.TxHash()
		signed, known := c.mine[txid]
		if !known {
			signed = signedBy(t.tx, c.b.signers, pub)
			c.mine[txid] = signed
		}
		if signed && !logged[unsignedHash(t.tx).String()] {
			missing = append(missing, txid.String())
		}
	}
	return missing
}

// adoptedKey is the log key of a transaction's action, read from its tags.
func adoptedKey(tx *wire.MsgTx) (string, bool) {
	var a action
	if op, ok := parseRelease(tx); ok {
		a = action{Kind: actionRelease, Deposit: op.String()}
	} else if id, ok := parsePayment(tx); ok {
		a = action{Kind: actionPayout, PegOut: id.String()}
	} else if op, ok := parseRefund(tx); ok {
		a = action{Kind: actionRefund, Deposit: op.String()}
	} else if op, ok := parseCancel(tx); ok {
		a = action{Kind: actionCancel, Deposit: op.String()}
	} else if op, ok := parseFence(tx); ok {
		a = action{Kind: actionFence, Deposit: op.String()}
	} else {
		return "", false
	}
	key, err := a.key()
	return key, err == nil
}

// adopt adds to c's log each transaction the chains show this key signed and
// the log lacks, under the action it did: for a key that signed before
// signers kept logs (the bridge once signed with every key itself). Only
// transactions in a block are adopted, dated when they were; one that isn't,
// or whose action its tags don't say, is left to investigate, and then
// nothing is written.
func (c *cosigner) adopt(s *pegState) ([]string, error) {
	missing := c.unlogged(s)
	if len(missing) == 0 {
		if _, err := c.fillAnchors(s, false); err != nil {
			return nil, err
		}
		return nil, c.log.write()
	}
	byID := map[string]chainTx{}
	for _, t := range append(append([]chainTx{}, s.vmTxs...), s.dogeTxs...) {
		byID[t.tx.TxHash().String()] = t
	}
	keys := make([]string, len(missing))
	for i, id := range missing {
		t := byID[id]
		if t.confirmations <= 0 {
			return nil, fmt.Errorf("%s is not in a block: an unlogged signature that may still confirm is never adopted; find out who signed it", id)
		}
		key, ok := adoptedKey(t.tx)
		if !ok {
			return nil, fmt.Errorf("%s: its tags don't say which action it did", id)
		}
		keys[i] = key
	}
	for i, id := range missing {
		t := byID[id]
		a := c.log.Actions[keys[i]]
		if a == nil {
			a = &loggedAction{First: t.time}
			c.log.Actions[keys[i]] = a
		}
		entry := loggedTx{Txid: unsignedHash(t.tx).String(), Signed: []string{id}}
		for _, in := range t.tx.TxIn {
			entry.Inputs = append(entry.Inputs, in.PreviousOutPoint.String())
		}
		a.Txs = append(a.Txs, entry)
	}
	// The anchors too: a log rebuilt from the chains starts where the
	// lines are now, not empty (anchorMoved refuses an empty one once a
	// line has moved).
	if _, err := c.fillAnchors(s, false); err != nil {
		return nil, err
	}
	return missing, c.log.write()
}

// tips is the tip of each chain's anchor line in s (lineage.go), by chain;
// a chain whose line has no root yet is left out.
func tips(s *pegState) (map[string]string, error) {
	out := map[string]string{}
	for chain, line := range map[string]*lineage{chainDogecoin: s.pegLine, chainDogecoinVM: s.reserveLine} {
		if line == nil {
			continue
		}
		tip, _, _, ok, err := line.tip()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", chain, err)
		}
		if ok {
			out[chain] = tip.String()
		}
	}
	return out, nil
}

// fillAnchors sets each chain's anchor in c's log that is empty to the tip
// of that chain's line, and checks each one already there leads to the
// tip; it changes nothing on a failure. Run by adopt and by signer-log
// anchors -set: every signer of a set must end up with the same anchors
// (deploy/upgrade-bridge.sh compares them), or one could follow a line of
// its own. An anchor set rather than reached by the signer's own
// signatures needs a cutover fence first (cutover.go).
//
// With refence, an anchor that no longer leads to the tip (the line ended
// at a spend that paid nothing back, or a reorganisation took the coin) is
// set to the tip too, and so needs a fence: the fence spends every coin
// the set holds, the old anchor included if it is still unspent, so
// whatever this signer signed from it can't confirm. That, not moving the
// anchor alone, is the recovery (RUNBOOK.md, "Anchor lineage").
func (c *cosigner) fillAnchors(s *pegState, refence bool) (map[string]string, error) {
	t, err := tips(s)
	if err != nil {
		return nil, err
	}
	set := map[string]string{}
	for chain, tip := range t {
		op, err := parseOutPoint(tip)
		if err != nil {
			return nil, err
		}
		line := s.pegLine
		if chain == chainDogecoinVM {
			line = s.reserveLine
		}
		probe := wire.NewMsgTx(1)
		probe.AddTxIn(wire.NewTxIn(&op, nil, nil))
		if c.log.Anchors[chain] != "" {
			err := c.log.anchorMoved(chain, probe, nil, line)
			if err == nil {
				continue
			}
			if !refence {
				return nil, fmt.Errorf("%s: the log's anchor doesn't lead to the line's tip %v: %w (RUNBOOK.md, \"Anchor lineage\")", chain, op, err)
			}
		}
		set[chain] = tip
	}
	if c.log.Anchors == nil {
		c.log.Anchors = map[string]string{}
	}
	if len(set) > 0 && c.log.Unfenced == nil {
		c.log.Unfenced = map[string]bool{}
	}
	for chain, tip := range set {
		c.log.Anchors[chain] = tip
		// Set, not reached by this signer's own signatures: whatever it
		// signed before is unknown to the log, so a fence comes first
		// (cutover.go).
		c.log.Unfenced[chain] = true
	}
	return t, nil
}

// walkedTips is, per chain, where the log's anchor leads following the
// line forward along confirmed spends (lineage.next, never a restart): the
// coin this signer would accept as the anchor now. It is the line's tip
// for a signer that agrees with the others; deploy/upgrade-bridge.sh
// compares it with the coordinator's tips. An anchor that leads nowhere
// shows as "error: ...".
func walkedTips(l *signingLog, s *pegState) map[string]string {
	out := map[string]string{}
	for chain, line := range map[string]*lineage{chainDogecoin: s.pegLine, chainDogecoinVM: s.reserveLine} {
		last := l.Anchors[chain]
		if last == "" || line == nil {
			continue
		}
		cur, err := parseOutPoint(last)
		if err != nil {
			out[chain] = "error: " + err.Error()
			continue
		}
		for range len(line.coins) + 2 {
			next, more, err := line.next(cur)
			if err != nil {
				out[chain] = "error: " + err.Error()
				break
			}
			if !more {
				out[chain] = cur.String()
				break
			}
			cur = next
		}
	}
	return out
}

// txids is every transaction in the log, by its unsigned txid.
func (l *signingLog) txids() map[string]bool {
	ids := map[string]bool{}
	for _, a := range l.Actions {
		for _, t := range a.Txs {
			ids[t.Txid] = true
		}
	}
	return ids
}

// quarantinePath is the marker that keeps a signer from signing until its
// operator has restored the signing log.
func quarantinePath(logPath string) string { return logPath + ".quarantine" }

// quarantined returns the reason the signer is quarantined, or "".
func (c *cosigner) quarantined() string {
	raw, err := os.ReadFile(quarantinePath(c.log.path))
	if errors.Is(err, os.ErrNotExist) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("the quarantine marker can't be read: %v", err)
	}
	return strings.TrimSpace(string(raw))
}

// quarantine writes the marker, so a restart stays quarantined too, and
// returns the error the signer refuses with.
func (c *cosigner) quarantine(reason string) error {
	path := quarantinePath(c.log.path)
	if err := os.WriteFile(path, []byte(reason+"\n"), 0o600); err != nil {
		c.b.logf("writing %s: %v", path, err)
	} else {
		_ = syncDir(filepath.Dir(path))
	}
	c.b.logf("QUARANTINED: %s", reason)
	return fmt.Errorf("%w: %s", errQuarantined, reason)
}

// fence quarantines the signer if the chains show it signed a transaction
// its log doesn't hold.
func (c *cosigner) fence(s *pegState) error {
	if reason := c.quarantined(); reason != "" {
		return fmt.Errorf("%w: %s (restore the signing log, then remove %s)", errQuarantined, reason, quarantinePath(c.log.path))
	}
	if missing := c.unlogged(s); len(missing) > 0 {
		return c.quarantine(fmt.Sprintf("the chains show this signer signed %s, which its signing log %s does not hold: "+
			"the log is older than what this key has signed (a restored backup or a copy)", strings.Join(missing, ", "), c.log.path))
	}
	return nil
}

// printTips prints the tips of the anchor lines as the coordinator's nodes
// see them.
func printTips(b *bridge, s *settings, signersPath, depositsPath string) error {
	signers, err := readSignerSet(signersPath)
	if err != nil {
		return err
	}
	if err := b.connect(s, signers); err != nil {
		return err
	}
	b.registry = registryFor(depositsPath, signersPath)
	b.contributionsPath = filepath.Join(filepath.Dir(signersPath), "contributions.txt")
	state, err := b.load()
	if err != nil {
		return err
	}
	t, err := tips(state)
	if err != nil {
		return err
	}
	printJSON(map[string]any{"tips": t})
	return nil
}

// syncDir makes a rename or a new file in dir durable.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// cmdSignerLog checks a signer's log against the chains, or starts one.
//
// check lists every transaction on either chain carrying the key's
// signature that the log lacks, and changes nothing: run it before
// upgrading a signer to a version that quarantines on them.
//
// adopt adds to the log (starting it if need be) what the key signed before
// signers kept logs: every transaction in a block that carries its signature
// and the log lacks, under the action its tags name. It adopts nothing if
// any such transaction isn't in a block, or doesn't say its action.
//
// init starts a log for a key that has none: a key made before signer keys
// came with a log, that has never signed. It refuses if the chains show a
// transaction the key signed: that key's log was lost, and must be restored
// from a backup instead.
func cmdSignerLog(args []string) error {
	const usage = "usage: dogevm signer-log check|init|adopt|anchors -signers FILE -key-file FILE [-log FILE]"
	if len(args) == 0 || (args[0] != "init" && args[0] != "check" && args[0] != "adopt" && args[0] != "anchors") {
		return errors.New(usage)
	}
	step := args[0]
	var s settings
	fs := flag.NewFlagSet("signer-log "+step, flag.ExitOnError)
	signersPath := fs.String("signers", "", "peg signer set file, with public keys only")
	keyFile := fs.String("key-file", "", "this signer's private key")
	depositsPath := fs.String("deposits", "", "this signer's deposit address registry (default: deposits.json next to -signers)")
	logPath := fs.String("log", "", "signing log (default: signing-log.json next to -key-file)")
	setAnchors := fs.Bool("set", false, "anchors: set the log's empty anchors to the lines' tips")
	refence := fs.Bool("refence", false, "anchors -set: also set an anchor that no longer leads to the tip, which then needs a cutover fence (RUNBOOK.md)")
	s.register(fs)
	b := bridgeFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	// A log checked or rebuilt against the wrong network would be wrong, and
	// the networks default to testnet: name them, by flag or environment
	// (a signer's signer.env sets both).
	named := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { named[f.Name] = true })
	if !named["doge-network"] && os.Getenv("DOGECOIN_NETWORK") == "" || !named["vm-network"] && os.Getenv("DOGEVM_NETWORK") == "" {
		return errors.New("name the networks: -doge-network and -vm-network, or DOGECOIN_NETWORK and DOGEVM_NETWORK (a signer's signer.env sets both)")
	}
	if err := s.resolve(); err != nil {
		return err
	}
	if *refence && !(step == "anchors" && *setAnchors) {
		return errors.New("-refence goes with anchors -set")
	}
	if step == "anchors" && *keyFile == "" {
		// The coordinator's view: the lines' tips alone, to compare with
		// the signers'.
		return printTips(b, &s, *signersPath, *depositsPath)
	}
	if err := required(map[string]string{"signers": *signersPath, "key-file": *keyFile}); err != nil {
		return err
	}
	if *logPath == "" {
		*logPath = filepath.Join(filepath.Dir(*keyFile), "signing-log.json")
	}
	if step == "init" || step == "adopt" || step == "anchors" && *setAnchors {
		// These write the log: the signer must be stopped, or one of its
		// signatures could be written over (lockSigningLog). Taken before
		// the log is read.
		lock, err := lockSigningLog(*logPath)
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	log := &signingLog{path: *logPath, Actions: map[string]*loggedAction{}}
	if step == "init" {
		if _, err := os.Lstat(*logPath); err == nil {
			return fmt.Errorf("%s already exists", *logPath)
		}
	} else if _, err := os.Lstat(*logPath); err == nil {
		// check: a missing log reads as empty, to show what init would find.
		if log, err = openSigningLog(*logPath); err != nil {
			return err
		}
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
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
	b.contributionsPath = filepath.Join(filepath.Dir(*signersPath), "contributions.txt")
	if err := watchPeg(b, false); err != nil {
		return err
	}
	state, err := b.load()
	if err != nil {
		return err
	}
	c := &cosigner{b: b, key: key, log: log}
	if step == "anchors" {
		if _, err := os.Lstat(*logPath); err != nil {
			return fmt.Errorf("there is no signing log at %s", *logPath)
		}
		var t map[string]string
		if *setAnchors {
			if t, err = c.fillAnchors(state, *refence); err != nil {
				return err
			}
			if err := log.write(); err != nil {
				return err
			}
		} else if t, err = tips(state); err != nil {
			return err
		}
		printJSON(map[string]any{"signingLog": *logPath, "anchors": log.Anchors, "tips": t,
			"walked": walkedTips(log, state), "needsFence": log.Unfenced})
		return nil
	}
	missing := c.unlogged(state)
	if step == "adopt" {
		adopted, err := c.adopt(state)
		if err != nil {
			return err
		}
		// Written even when the chains show nothing to adopt: a signer won't
		// start without its log.
		if err := log.write(); err != nil {
			return err
		}
		printJSON(map[string]any{"signingLog": *logPath, "adopted": adopted})
		return nil
	}
	if step == "check" {
		_, statErr := os.Lstat(*logPath)
		quarantined := c.quarantined()
		printJSON(map[string]any{"signingLog": *logPath, "exists": statErr == nil, "quarantined": quarantined, "unlogged": missing})
		switch {
		case statErr != nil:
			return fmt.Errorf("there is no signing log at %s", *logPath)
		case quarantined != "":
			return fmt.Errorf("this signer is quarantined: %s", quarantined)
		case len(missing) > 0:
			return fmt.Errorf("the log lacks %d transaction(s) the chains show this key signed", len(missing))
		}
		return nil
	}
	if len(missing) > 0 {
		return fmt.Errorf("the chains show this key signed %s: its signing log was lost; restore it from a backup",
			strings.Join(missing, ", "))
	}
	if _, err := createSigningLog(*logPath); err != nil {
		return err
	}
	printJSON(map[string]string{"signingLog": *logPath})
	return nil
}
