package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Dead history (Round 6, V2; FENCE-NAMES.md "Dead history is durable").
//
// A cutover must cover every coin a signature given before it could spend,
// including the outputs of a transaction a reorganisation took out of its
// block. Dogecoin Core 1.14 has no listsinceblock include_removed, and a
// wallet that is rebuilt, or a transaction the wallet stops listing, would
// take that evidence with it. So every wallet transaction that isn't in a
// block rootDepth deep is written, body and all, to a file beside the
// signer set (unsettledFor: a directory the web server can't write), and
// read back on every history read, full rereads and restarts included. One
// the wallet no longer lists comes back as conflicted, at the depth of the
// best current evidence that it can never be mined (deadproof.go): its
// outputs stay recorded, and a cutover stays gated, until that evidence is
// rootDepth deep. Disappearing proves nothing: it may still be mined. A
// conflicted transaction stays in the file once its conflict is deep (Round
// 8, X1): that conflict is what clears its outputs, so its record is kept
// as a tombstone rather than deleted.

// deadHistoryFile is the file name beside the registry.
const deadHistoryFile = "dogecoin-unsettled.json"

// deadRecord is one unsettled transaction.
type deadRecord struct {
	Hex string `json:"hex"`
	// ConflictDepth is the deepest the wallet ever saw a conflict of it.
	ConflictDepth int64 `json:"conflictDepth,omitempty"`
	Time          int64 `json:"time,omitempty"`
	// Proof is an operator-recorded spender of one of its inputs, checked
	// against the chain on every read (deadproof.go).
	Proof *conflictProof `json:"proof,omitempty"`
	// Settled is set once the wallet listed it conflicted rootDepth deep: a
	// reorganisation shallower than that can't undo the conflict, so the
	// record stays as a tombstone saying so, even if the wallet later
	// stops listing it and lists nothing that conflicts with it (Round 8,
	// X1).
	Settled bool `json:"settled,omitempty"`
}

// deadHistory is the file and what this process last read of it.
type deadHistory struct {
	path  string
	depth int64 // what settles a transaction: rootDepth on Dogecoin
}

func (d *deadHistory) lock() (func(), error) {
	f, err := os.OpenFile(d.path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("locking %s: %w", d.path, err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking %s: %w", d.path, err)
	}
	return func() { _ = unlockFile(f); f.Close() }, nil
}

func (d *deadHistory) read() (map[string]deadRecord, error) {
	raw, err := os.ReadFile(d.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]deadRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	records := map[string]deadRecord{}
	if err := json.Unmarshal(raw, &records); err != nil {
		return nil, fmt.Errorf("%s: %w", d.path, err)
	}
	return records, nil
}

func (d *deadHistory) write(records map[string]deadRecord) error {
	raw, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(d.path), ".unsettled-*")
	if err != nil {
		return err
	}
	_, err = tmp.Write(append(raw, '\n'))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), d.path)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return syncDir(filepath.Dir(d.path))
}

// sync brings the file up to date with the wallet's entries at tip, and
// returns every record the wallet no longer lists, for the history read to
// add back. body reads a listed transaction's bytes. Under the file's lock,
// so the bridge and a signer-log command run beside it don't lose each
// other's records; each process only adds what it saw unsettled and
// removes what it saw settle.
func (d *deadHistory) sync(tip int64, entries map[string]historyEntry, body func(string) (string, error)) (map[string]deadRecord, error) {
	unlock, err := d.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	records, err := d.read()
	if err != nil {
		return nil, err
	}
	changed := false
	ids := make([]string, 0, len(entries))
	for txid := range entries {
		ids = append(ids, txid)
	}
	sort.Strings(ids)
	for _, txid := range ids {
		e := entries[txid]
		// Only a transaction in a block rootDepth deep leaves the file: the
		// wallet keeps listing it. A conflicted one stays (Round 8, X1): its
		// conflict is what clears its outputs in a cutover, and the wallet
		// may stop listing it.
		settled := e.height > 0 && tip-e.height+1 >= d.depth
		rec, have := records[txid]
		if settled {
			if have {
				delete(records, txid)
				changed = true
			}
			continue
		}
		if !have {
			hex, err := body(txid)
			if err != nil {
				return nil, fmt.Errorf("keeping unsettled transaction %s: %w", txid, err)
			}
			rec = deadRecord{Hex: hex, Time: e.time}
			changed = true
		}
		if e.conflicted && e.conflictDepth > rec.ConflictDepth {
			rec.ConflictDepth = e.conflictDepth
			changed = true
		}
		if e.conflicted && e.conflictDepth >= d.depth && !rec.Settled {
			rec.Settled = true
			changed = true
		}
		records[txid] = rec
	}
	if changed {
		if err := d.write(records); err != nil {
			return nil, err
		}
	}
	gone := map[string]deadRecord{}
	for txid, rec := range records {
		if _, listed := entries[txid]; !listed {
			gone[txid] = rec
		}
	}
	return gone, nil
}
