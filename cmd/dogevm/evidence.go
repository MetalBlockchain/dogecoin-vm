package main

// Chain evidence for a signer's log. The log names each transaction by its
// ID without signatures (what the signer signed), but signing a P2SH input,
// as every peg input on both chains is, changes a transaction's ID: the
// chains list it under another. So a signer matches what the chains list
// to its log by the ID with signatures stripped (unsignedHash), and records
// the IDs it sees there (loggedTx.Signed), so it can still ask its node
// about a transaction its wallet later stops listing.

import (
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

type chainEvidence struct {
	// listed is, per chain, every transaction s read, by unsigned ID.
	listed map[string]map[string]chainTx
	nodes  func(chain string) chain
	// deep is the Dogecoin depth past which a reorganisation can't take a
	// block away (rootDepth); 0 treats any block as final (Round 7, W1).
	deep int64
}

func newChainEvidence(s *pegState, nodes func(string) chain) *chainEvidence {
	e := &chainEvidence{listed: map[string]map[string]chainTx{}, nodes: nodes}
	for name, txs := range map[string][]chainTx{chainDogecoinVM: s.vmTxs, chainDogecoin: s.dogeTxs} {
		byID := map[string]chainTx{}
		for _, t := range txs {
			id := unsignedHash(t.tx).String()
			// A confirmed listing wins over an unconfirmed one (a
			// replacement, or a copy whose signatures someone changed).
			if prev, ok := byID[id]; !ok || t.confirmations > prev.confirmations {
				byID[id] = t
			}
		}
		e.listed[name] = byID
	}
	return e
}

// confirmed reports whether t, a transaction this signer logged, is in a
// block on chain: as listed, or, asking the node, under any ID this signer
// has seen it under, or its unsigned one.
func (e *chainEvidence) confirmed(chain string, t loggedTx) bool {
	if lt, ok := e.listed[chain][t.Txid]; ok && lt.confirmations > 0 {
		return true
	}
	if e.nodes == nil {
		return false
	}
	f := confirmedOn(e.nodes(chain))
	if f == nil {
		return false
	}
	for _, id := range t.Signed {
		if f(id) {
			return true
		}
	}
	return f(t.Txid)
}

// shallow reports whether t, confirmed on chain, could still be taken out of
// its block by a reorganisation: a Dogecoin transaction listed fewer than
// deep blocks down. DogecoinVM blocks are final. One the node says is in a
// block but the wallet doesn't list is taken as deep, as before: the depth
// can't be read.
func (e *chainEvidence) shallow(chain string, t loggedTx) bool {
	if chain != chainDogecoin || e.deep <= 1 {
		return false
	}
	lt, ok := e.listed[chain][t.Txid]
	return ok && lt.confirmations > 0 && lt.confirmations < e.deep
}

// spentElsewhere reports whether a confirmed transaction other than t (by
// unsigned ID) spends one of t's inputs on chain, as listed: t can then
// never confirm.
func (e *chainEvidence) spentElsewhere(chain string, t loggedTx) bool {
	ins := map[string]bool{}
	for _, in := range t.Inputs {
		if op, err := parseOutPoint(in); err == nil {
			ins[op.String()] = true
		}
	}
	for id, lt := range e.listed[chain] {
		if lt.confirmations <= 0 || id == t.Txid {
			continue
		}
		for _, in := range lt.tx.TxIn {
			if ins[in.PreviousOutPoint.String()] {
				return true
			}
		}
	}
	return false
}

// learnSigned records in the log every ID the chains list one of its
// transactions under, and writes it if anything was new.
func (c *cosigner) learnSigned(e *chainEvidence) error {
	changed := false
	for _, a := range c.log.Actions {
		for i := range a.Txs {
			t := &a.Txs[i]
			for _, byID := range e.listed {
				lt, ok := byID[t.Txid]
				if !ok {
					continue
				}
				id := lt.tx.TxHash().String()
				if !contains(t.Signed, id) {
					t.Signed = append(t.Signed, id)
					changed = true
				}
			}
		}
	}
	if !changed {
		return nil
	}
	return c.log.write()
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// releasesDead reports whether every release of deposit op this signer
// signed is provably dead on DogecoinVM: none is in a block, and a
// confirmed transaction other than it spends one of its inputs.
// DogecoinVM's blocks are final, so a dead one stays dead.
func (c *cosigner) releasesDead(e *chainEvidence, op wire.OutPoint) bool {
	a := c.log.Actions[actionRelease+":"+op.String()]
	if a == nil {
		return true
	}
	for _, t := range a.Txs {
		if e.confirmed(chainDogecoinVM, t) || !e.spentElsewhere(chainDogecoinVM, t) {
			return false
		}
	}
	return true
}

// releasesDeadOrConflicting reports whether every release of deposit op
// this signer signed is dead (releasesDead's test) or spends anchor, the
// coin a cancellation of op spends first: none is in a block, and none
// could confirm beside that cancellation.
func (c *cosigner) releasesDeadOrConflicting(e *chainEvidence, op, anchor wire.OutPoint) bool {
	a := c.log.Actions[actionRelease+":"+op.String()]
	if a == nil {
		return true
	}
	for _, t := range a.Txs {
		if e.confirmed(chainDogecoinVM, t) {
			return false
		}
		if e.spentElsewhere(chainDogecoinVM, t) || contains(t.Inputs, anchor.String()) {
			continue
		}
		return false
	}
	return true
}
