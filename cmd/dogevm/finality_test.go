package main

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFinalityMeter(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "finality.json")
	m := newFinalityMeter(path)
	require.Nil(m.summary())

	t0 := time.Unix(1_000_000, 0)
	for i, ms := range []int64{900, 1200, 1500, 1100, 3000} {
		id := fmt.Sprintf("tx%d", i)
		m.received(id, t0)
		m.final([]string{"unrelated", id}, t0.Add(time.Duration(ms)*time.Millisecond))
	}
	// A refused payment, and one never seen in a block, add nothing.
	m.received("refused", t0)
	m.forget("refused")
	m.final([]string{"refused"}, t0.Add(time.Second))
	m.received("pending", t0)

	s := m.summary()
	require.Equal(5, s.Payments)
	require.Equal(int64(1200), s.MedianMs) // 900 1100 [1200] 1500 3000
	require.Equal(int64(3000), s.P90Ms)
	require.Equal(int64(3000), s.LatestMs)

	// The figures survive a restart.
	require.Equal(s, newFinalityMeter(path).summary())

	// Only the latest finalitySamples count.
	for i := 0; i < finalitySamples+10; i++ {
		id := fmt.Sprintf("more%d", i)
		m.received(id, t0)
		m.final([]string{id}, t0.Add(2*time.Second))
	}
	require.Equal(finalitySamples, m.summary().Payments)
	require.Equal(int64(2000), m.summary().MedianMs)
}

func TestFinalityMeterNil(t *testing.T) {
	var m *finalityMeter // Dogecoin index off: no meter
	m.received("x", time.Now())
	m.final([]string{"x"}, time.Now())
	m.forget("x")
	require.Nil(t, m.summary())
}
