package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"sort"
)

// cmdUnsettled lists a node's unsettled transactions, or records a proof
// that a dropped one can never be mined (deadproof.go):
//
//	dogevm unsettled list  -signers FILE
//	dogevm unsettled prove -signers FILE -txid TXID -spender HEX -block HASH
//
// -signers is the signer set beside the file: the coordinator's
// separate-signers/signers.json, or a signer's own signers.json; each node
// keeps its own file, so a proof is recorded on each. prove checks the proof
// against this node's chain before recording it, and every later read checks
// it again: a wrong proof clears nothing. Safe while the node runs (the file
// has its own lock).
func cmdUnsettled(args []string) error {
	const usage = "usage: dogevm unsettled list|prove -signers FILE [-txid TXID -spender HEX -block HASH]"
	if len(args) == 0 || args[0] != "list" && args[0] != "prove" {
		return errors.New(usage)
	}
	step := args[0]
	var s settings
	fs := flag.NewFlagSet("unsettled "+step, flag.ExitOnError)
	signersPath := fs.String("signers", "", "the signer set beside the unsettled file (public keys are enough)")
	txid := fs.String("txid", "", "prove: the dropped transaction")
	spender := fs.String("spender", "", "prove: a transaction spending one of its inputs, hex")
	block := fs.String("block", "", "prove: the hash of the block holding the spender")
	s.register(fs)
	b := bridgeFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	named := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { named[f.Name] = true })
	if !named["doge-network"] && os.Getenv("DOGECOIN_NETWORK") == "" || !named["vm-network"] && os.Getenv("DOGEVM_NETWORK") == "" {
		return errors.New("name the networks: -doge-network and -vm-network, or DOGECOIN_NETWORK and DOGEVM_NETWORK")
	}
	if err := s.resolve(); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath}); err != nil {
		return err
	}
	if step == "prove" {
		if err := required(map[string]string{"txid": *txid, "spender": *spender, "block": *block}); err != nil {
			return err
		}
		if !validHex(*spender) {
			return errors.New("-spender must be the transaction's hex")
		}
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	if err := b.connect(&s, signers); err != nil {
		return err
	}
	b.unsettledPath = unsettledFor(*signersPath)
	b.keepUnsettled()
	dc, ok := b.doge.(*dogeChain)
	if !ok || dc.dead == nil {
		return errors.New("no Dogecoin node to read")
	}
	records, err := dc.dead.read()
	if err != nil {
		return err
	}
	tip, entries, err := dc.walletEntries()
	if err != nil {
		return err
	}
	if step == "prove" {
		rec, ok := records[*txid]
		if !ok {
			return fmt.Errorf("%s has no unsettled transaction %s (list shows what it has)", dc.dead.path, *txid)
		}
		dropped, err := decodeTx(rec.Hex)
		if err != nil {
			return err
		}
		p := conflictProof{Spender: *spender, Block: *block}
		depth, err := dc.proofDepth(dropped, &p)
		if err != nil {
			return err
		}
		if depth < 1 {
			return fmt.Errorf("block %s isn't on this node's main chain", *block)
		}
		if err := dc.dead.setProof(*txid, p); err != nil {
			return err
		}
		printJSON(map[string]any{"file": dc.dead.path, "txid": *txid, "conflictDepth": depth,
			"settlesAt": dc.dead.depth, "settledNow": depth >= dc.dead.depth})
		return nil
	}
	type row struct {
		TxID          string `json:"txid"`
		Listed        bool   `json:"listedByWallet"`
		ConflictDepth int64  `json:"conflictDepth"`
		Proof         bool   `json:"proof"`
		ProofError    string `json:"proofError,omitempty"`
	}
	var rows []row
	for id, rec := range records {
		r := row{TxID: id, Proof: rec.Proof != nil}
		_, r.Listed = entries[id]
		if dropped, err := decodeTx(rec.Hex); err == nil {
			r.ConflictDepth, _ = dc.walletConflictDepth(tip, dropped, entries)
			if rec.Proof != nil {
				pd, perr := dc.proofDepth(dropped, rec.Proof)
				if perr != nil {
					r.ProofError = perr.Error()
				}
				r.ConflictDepth = max(r.ConflictDepth, pd)
			}
		}
		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].TxID < rows[j].TxID })
	printJSON(map[string]any{"file": dc.dead.path, "settlesAt": dc.dead.depth, "unsettled": rows})
	return nil
}
