package main

import (
	"encoding/json"
	"errors"
	"log"
	"os"
	"slices"
	"sync"
	"time"
)

// The finality meter measures how long DogecoinVM takes to make a payment
// final, live: from when this server receives the payment (POST /api/tx) to
// when the block holding it is accepted. The node records only accepted
// blocks, and on DogecoinVM an accepted block is final. The block watcher
// checks four times a second, so a measurement can run up to 250 ms long,
// never short.

// finalitySamples is how many recent payments the figures cover.
const finalitySamples = 50

type finalityMeter struct {
	mu      sync.Mutex
	sent    map[string]time.Time // txid -> when it was received
	samples []int64              // milliseconds, oldest first
	path    string               // where samples are kept across restarts; "" for memory only
}

// finalitySummary is what /api/status reports.
type finalitySummary struct {
	Payments int   `json:"payments"`
	MedianMs int64 `json:"medianMs"`
	P90Ms    int64 `json:"p90Ms"`
	LatestMs int64 `json:"latestMs"`
}

func newFinalityMeter(path string) *finalityMeter {
	m := &finalityMeter{sent: map[string]time.Time{}, path: path}
	if path == "" {
		return m
	}
	raw, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(raw, &m.samples); err != nil {
			log.Printf("finality: ignoring %s: %v", path, err)
			m.samples = nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		log.Printf("finality: %v", err)
	}
	return m
}

// received starts the clock for a payment.
func (m *finalityMeter) received(txid string, at time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Forget payments that never made it into a block.
	for id, t := range m.sent {
		if at.Sub(t) > 10*time.Minute {
			delete(m.sent, id)
		}
	}
	if len(m.sent) < 10000 {
		m.sent[txid] = at
	}
}

// forget drops a payment the node refused.
func (m *finalityMeter) forget(txid string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sent, txid)
}

// final stops the clock for any of txids, whose block was accepted at at.
func (m *finalityMeter) final(txids []string, at time.Time) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	measured := false
	for _, id := range txids {
		t, ok := m.sent[id]
		if !ok {
			continue
		}
		delete(m.sent, id)
		m.samples = append(m.samples, max(at.Sub(t).Milliseconds(), 0))
		measured = true
	}
	if !measured {
		return
	}
	if n := len(m.samples); n > finalitySamples {
		m.samples = slices.Clone(m.samples[n-finalitySamples:])
	}
	m.save()
}

func (m *finalityMeter) save() {
	if m.path == "" {
		return
	}
	raw, _ := json.Marshal(m.samples)
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		log.Printf("finality: %v", err)
		return
	}
	if err := os.Rename(tmp, m.path); err != nil {
		log.Printf("finality: %v", err)
	}
}

// summary returns the figures, or nil before the first measurement.
func (m *finalityMeter) summary() *finalitySummary {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(m.samples)
	if n == 0 {
		return nil
	}
	sorted := slices.Sorted(slices.Values(m.samples))
	// Nearest rank: the smallest value at least p of the samples reach.
	rank := func(p float64) int64 {
		i := int(p*float64(n)+0.999999) - 1
		return sorted[min(max(i, 0), n-1)]
	}
	return &finalitySummary{
		Payments: n,
		MedianMs: rank(0.5),
		P90Ms:    rank(0.9),
		LatestMs: m.samples[n-1],
	}
}
