package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLegacyWaiverIsUndoneOnLoad (W3): round 5's waiver deleted a coin from
// the cutover record and listed it under "cutoverWaived"; a log that then
// finished its cutover reads "fenced" with no record. Loading such a log puts
// the coin back, so it is in a cutover again (signs only fences) until the
// coin is cleared the normal way.
func TestLegacyWaiverIsUndoneOnLoad(t *testing.T) {
	require := require.New(t)
	path := filepath.Join(t.TempDir(), "signing-log.json")
	coin := "aa00000000000000000000000000000000000000000000000000000000000000:1"
	doc := map[string]any{
		"actions":       map[string]any{},
		"history":       historyFenced,
		"cutoverEpoch":  1,
		"cutoverWaived": map[string][]string{chainDogecoin: {coin}},
	}
	raw, err := json.Marshal(doc)
	require.NoError(err)
	require.NoError(os.WriteFile(path, raw, 0o600))

	l, err := openSigningLog(path)
	require.NoError(err)
	require.Equal([]string{coin}, l.Cutover[chainDogecoin])
	require.True(l.unproven(), "back in a cutover: signs only fences")

	// A waived coin naming an unknown chain, or a malformed one, refuses to load.
	doc["cutoverWaived"] = map[string][]string{"bitcoin": {coin}}
	raw, _ = json.Marshal(doc)
	require.NoError(os.WriteFile(path, raw, 0o600))
	_, err = openSigningLog(path)
	require.ErrorContains(err, "unknown chain")
	doc["cutoverWaived"] = map[string][]string{chainDogecoin: {"nonsense"}}
	raw, _ = json.Marshal(doc)
	require.NoError(os.WriteFile(path, raw, 0o600))
	_, err = openSigningLog(path)
	require.Error(err)
}
