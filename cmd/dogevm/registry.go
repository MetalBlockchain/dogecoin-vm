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
type depositRegistry struct {
	path       string
	maxEntries int
	mu         sync.Mutex
}

const (
	// These defaults bound how quickly and how far an exposed service can grow
	// the permanent watch set. Operators can choose lower or higher limits.
	defaultMaxRegistrations  = 600
	defaultMaxDepositEntries = 10_000
)

var errDepositRegistryFull = errors.New("deposit address registry is full")

// setMaxEntries sets the permanent registry ceiling. Existing entries remain
// usable if an operator lowers the ceiling below the current size.
func (r *depositRegistry) setMaxEntries(max int) {
	r.mu.Lock()
	r.maxEntries = max
	r.mu.Unlock()
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

// capacityFor reports how many distinct entries in ds are new and refuses the
// whole batch if it would cross the registry ceiling.
func (r *depositRegistry) capacityFor(ds []destination) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dests, err := r.read()
	if err != nil {
		return 0, err
	}
	known := make(map[destination]struct{}, len(dests)+len(ds))
	for _, d := range dests {
		known[d] = struct{}{}
	}
	newEntries := 0
	for _, d := range ds {
		if _, ok := known[d]; ok {
			continue
		}
		known[d] = struct{}{}
		newEntries++
	}
	if r.maxEntries > 0 && len(dests)+newEntries > r.maxEntries {
		return 0, fmt.Errorf("%w (limit %d)", errDepositRegistryFull, r.maxEntries)
	}
	return newEntries, nil
}

// add records d, reporting whether it was new.
func (r *depositRegistry) add(d destination) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	dests, err := r.read()
	if err != nil {
		return false, err
	}
	for _, existing := range dests {
		if existing == d {
			return false, nil
		}
	}
	if r.maxEntries > 0 && len(dests) >= r.maxEntries {
		return false, fmt.Errorf("%w (limit %d)", errDepositRegistryFull, r.maxEntries)
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
