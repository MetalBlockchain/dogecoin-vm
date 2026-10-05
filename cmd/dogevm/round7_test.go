package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/mempool"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
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

// TestShallowCheapFenceKeepsTheCostlierFeeReserved (W1, Codex r7 #1): a
// signer signed a cheap Dogecoin fence and a costlier replacement. While the
// cheap one is in a block shallower than rootDepth, a reorganisation could
// mine the costlier one instead: the difference stays reserved against the
// surplus. Once the cheap one is rootDepth deep, nothing stays reserved.
func TestShallowCheapFenceKeepsTheCostlierFeeReserved(t *testing.T) {
	require := require.New(t)
	cheapID := "1111111111111111111111111111111111111111111111111111111111111111"
	in := "2222222222222222222222222222222222222222222222222222222222222222:0"
	log := &signingLog{Actions: map[string]*loggedAction{
		"fence:dogecoin:1": {Txs: []loggedTx{
			{Txid: cheapID, Chain: chainDogecoin, Fee: 10_000_000, Inputs: []string{in}},
			{Txid: "3333333333333333333333333333333333333333333333333333333333333333", Chain: chainDogecoin, Fee: 200_000_000, Inputs: []string{in}},
		}},
	}}
	cheap := wire.NewMsgTx(1)
	ev := func(confs int64) *chainEvidence {
		return &chainEvidence{deep: 20, listed: map[string]map[string]chainTx{
			chainDogecoin: {cheapID: {tx: cheap, confirmations: confs}},
		}}
	}
	none := func(string, wire.OutPoint) bool { return false }
	require.EqualValues(190_000_000, log.unseenFees(nil, ev(3), none, ""), "shallow: the costlier fee less the one paid stays reserved")
	require.EqualValues(190_000_000, log.unseenFees(nil, ev(19), none, ""))
	require.Zero(log.unseenFees(nil, ev(20), none, ""), "rootDepth deep: the costlier one can't confirm")
	vm := &chainEvidence{deep: 20, listed: map[string]map[string]chainTx{chainDogecoinVM: {cheapID: {tx: cheap, confirmations: 1}}}}
	log.Actions["fence:dogecoin:1"].Txs[0].Chain = chainDogecoinVM
	log.Actions["fence:dogecoin:1"].Txs[1].Chain = chainDogecoinVM
	require.Zero(log.unseenFees(nil, vm, none, ""), "a DogecoinVM block is final")
}

// TestVMFenceStaysRelayable (W4, Codex r7 #4): a fence's signed size is
// bounded so DogecoinVM's mempool (btcd's standardness, 400,000 weight)
// takes it. A full 400-input fence of the reserve's 2-of-3 coins, signed,
// is refused; trimToSize keeps the next one under the limit, and that one
// passes the plugin's own policy check with real-size signatures.
func TestVMFenceStaysRelayable(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	b := h.b
	coins := make([]utxo, 400)
	for i := range coins {
		coins[i] = utxo{outPoint: wire.OutPoint{Hash: chainhash.Hash{byte(i), byte(i >> 8), 7}, Index: uint32(i)},
			value: doge, pkScript: b.signers.pkScript()}
	}
	s := &pegState{}
	signed := func(inputs []utxo) *btcutil.Tx {
		size, err := b.fenceSizeOf(s, chainDogecoinVM, inputs)
		require.NoError(err)
		tx, err := b.buildFence(chainDogecoinVM, inputs, fenceFee(chainDogecoinVM, b.vmFee, size))
		require.NoError(err)
		sig := bytes.Repeat([]byte{0x30}, 72) // a DER signature's size with its hash type
		for _, in := range tx.TxIn {
			sb := txscript.NewScriptBuilder().AddOp(txscript.OP_0)
			for range b.signers.Required {
				sb.AddData(sig)
			}
			in.SignatureScript, err = sb.AddData(b.signers.redeemScript).Script()
			require.NoError(err)
		}
		require.LessOrEqual(int64(tx.SerializeSize()), size, "the estimate covers the signed size")
		return btcutil.NewTx(tx)
	}
	check := func(tx *btcutil.Tx) error {
		return mempool.CheckTransactionStandard(tx, 1, time.Now(), 0, wire.TxVersion)
	}
	require.ErrorContains(check(signed(coins)), "weight", "400 inputs: over the limit, whatever the fee")

	trimmed, err := b.trimToSize(s, chainDogecoinVM, coins)
	require.NoError(err)
	require.Less(len(trimmed), len(coins))
	require.Equal(coins[0].outPoint, trimmed[0].outPoint, "the anchor stays first")
	require.NoError(check(signed(trimmed)), "%d inputs", len(trimmed))
	require.Error(check(signed(coins[:len(trimmed)+40])), "well past the trimmed size is refused")
}
