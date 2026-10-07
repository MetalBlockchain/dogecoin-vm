package main

import (
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDepositRegistryCeiling(t *testing.T) {
	require := require.New(t)
	r := &depositRegistry{path: filepath.Join(t.TempDir(), "deposits.json"), maxEntries: 2}
	first := destination{kind: destP2PKH, hash: [20]byte{1}}
	second := destination{kind: destP2PKH, hash: [20]byte{2}}
	third := destination{kind: destP2PKH, hash: [20]byte{3}}

	added, err := r.add(first)
	require.NoError(err)
	require.True(added)
	added, err = r.add(second)
	require.NoError(err)
	require.True(added)
	added, err = r.add(first)
	require.NoError(err)
	require.False(added, "an existing address remains free at the ceiling")

	_, err = r.add(third)
	require.ErrorIs(err, errDepositRegistryFull)
	entries, err := r.list()
	require.NoError(err)
	require.Equal([]destination{first, second}, entries)
}

func TestRateLimitBoundsKeys(t *testing.T) {
	require := require.New(t)
	global := newRateLimit(2, time.Hour)
	require.True(global.allow("new-addresses"))
	require.True(global.allow("new-addresses"))
	require.False(global.allow("new-addresses"), "the global registration budget must be enforced")

	l := newRateLimit(1, time.Hour)
	for i := 0; i < maxRateLimitKeys; i++ {
		require.True(l.allow(strconv.Itoa(i)))
	}
	require.False(l.allow("one-too-many"))
	require.Len(l.events, maxRateLimitKeys)

	// Cleanup makes room once entries have expired.
	for key := range l.events {
		l.events[key] = []time.Time{time.Now().Add(-2 * time.Hour)}
	}
	l.nextCleanup = time.Time{}
	require.True(l.allow("after-cleanup"))
	require.Len(l.events, 1)
}
