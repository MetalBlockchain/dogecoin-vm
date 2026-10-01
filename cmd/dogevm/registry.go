package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// depositRegistry records the DogecoinVM destinations that have personal
// deposit addresses, so the bridge knows which Dogecoin addresses to watch.
// It is a JSON array of hex "kind || hash160" strings. Losing it loses no
// funds: re-registering a destination recreates the same address.
//
// The bridge and the web server each add to the same file, from separate
// processes, so an addition holds an exclusive lock on a sibling file
// (deposits.json.lock, in the registry's directory, which both may write)
// across reading the file and replacing it: otherwise two at once could
// each write the list without the other's entry.
type depositRegistry struct {
	path string
	mu   sync.Mutex
}

func (r *depositRegistry) list() ([]destination, error) {
	if r == nil {
		return nil, nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read()
}

func (r *depositRegistry) read() ([]destination, error) {
	raw, err := os.ReadFile(r.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []string
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("%s: %w", r.path, err)
	}
	dests := make([]destination, 0, len(entries))
	for _, e := range entries {
		payload, err := hex.DecodeString(e)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", r.path, err)
		}
		d, err := decodeDestination(payload)
		if err != nil {
			return nil, fmt.Errorf("%s: %q: %w", r.path, e, err)
		}
		dests = append(dests, d)
	}
	return dests, nil
}

// add records d, reporting whether it was new.
// has reports whether d is registered.
func (r *depositRegistry) has(d destination) (bool, error) {
	dests, err := r.list()
	if err != nil {
		return false, err
	}
	for _, existing := range dests {
		if existing == d {
			return true, nil
		}
	}
	return false, nil
}

// lock takes the registry's cross-process lock; the returned function
// releases it.
func (r *depositRegistry) lock() (func(), error) {
	f, err := os.OpenFile(r.path+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("locking the deposit registry: %w", err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return nil, fmt.Errorf("locking the deposit registry: %w", err)
	}
	return func() { _ = unlockFile(f); f.Close() }, nil
}

func (r *depositRegistry) add(d destination) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	unlock, err := r.lock()
	if err != nil {
		return false, err
	}
	defer unlock()

	// Read under the lock: what another process added is in this copy.
	dests, err := r.read()
	if err != nil {
		return false, err
	}
	for _, existing := range dests {
		if existing == d {
			return false, nil
		}
	}
	dests = append(dests, d)

	entries := make([]string, len(dests))
	for i, d := range dests {
		entries[i] = hex.EncodeToString(append([]byte{d.kind}, d.hash[:]...))
	}
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return false, err
	}
	// Write then rename, so the bridge never reads a partial file.
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".deposits-*")
	if err != nil {
		return false, err
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return false, err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return false, err
	}
	return true, os.Rename(tmp.Name(), r.path)
}
