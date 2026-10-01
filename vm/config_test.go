// Copyright (C) 2024-2025, Metallicus, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package vm

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/MetalBlockchain/dogecoin-vm/btcd"
	"github.com/stretchr/testify/require"
)

// TestChainConfigRefusesConsensusKeys: a node's chain config can't change
// the network, the peg reserve or checkpoints, however the key is spelled,
// nor drop an index at every start; ordinary settings pass.
func TestChainConfigRefusesConsensusKeys(t *testing.T) {
	for _, raw := range []string{
		`{"mainNet": true}`, `{"MainNet": true}`, `{"PEGRESERVEADDRESS": "x"}`,
		`{"addCheckpoints": ["1:00"]}`, `{"regressionTest": true}`, `{"sigNetChallenge": "51"}`,
		`{"dropAddrIndex": true}`, `{"droptxindex": true}`,
	} {
		require.Error(t, applyChainConfig(&btcd.Config{}, []byte(raw)), raw)
	}
	require.NoError(t, applyChainConfig(&btcd.Config{}, []byte(`{"txIndex": true, "addrIndex": true, "rpcUser": "u"}`)))
}

// TestINIFileCantChangeConsensus: btcd's own config file, which the chain
// config denylist never sees, can't change consensus settings either.
func TestINIFileCantChangeConsensus(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	ini := filepath.Join(dir, "evil.conf")
	require.NoError(t, os.WriteFile(ini, []byte("addcheckpoint=1:00000000000000000000000000000000000000000000000000000000000000aa\n"), 0o600))
	_, err := startTestVM(t, dir, map[string]any{"configFile": ini})
	require.ErrorContains(t, err, "changes consensus settings")
}
