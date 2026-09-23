package main

import (
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// TestWebChainMatchesGo runs the web wallet's chain.js under Node and checks
// it agrees with the Go implementation: addresses, WIF keys, personal
// deposit addresses, and that a transaction it signs verifies in btcd's
// script engine.
func TestWebChainMatchesGo(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	require := require.New(t)

	vmParams, _ := dogevmParams("testnet")
	dogeParams := &dogecoinTestNet
	signers, err := newSignerSet(2, 3)
	require.NoError(err)

	key, _ := btcec.PrivKeyFromBytes([]byte("0123456789abcdef0123456789abcdef"))
	report, err := describeKey(key, vmParams, dogeParams)
	require.NoError(err)
	vmAddr, err := p2pkhAddress(key, vmParams)
	require.NoError(err)
	dest, err := destinationOf(vmAddr)
	require.NoError(err)
	wantDeposit, err := signers.depositAddress(dest, dogeParams)
	require.NoError(err)

	// A UTXO of 500 DOGE the key owns, paid on to the peg reserve with a
	// DVMO tag, as the Withdraw tab does.
	fromScript := destinationScript(vmAddr)
	dogeDest := destination{kind: destP2PKH, hash: [20]byte{9}}

	// The transaction that created the UTXO; the page fetches it to check
	// the UTXO's value.
	prev := wire.NewMsgTx(1)
	prev.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 7}, []byte{0x51}, nil))
	for i := 0; i < 3; i++ {
		prev.AddTxOut(wire.NewTxOut(koinuPerDoge, []byte{0x51}))
	}
	prev.AddTxOut(wire.NewTxOut(500*koinuPerDoge, fromScript))
	other := wire.NewMsgTx(1)
	other.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 8}, []byte{0x51}, nil))
	for i := 0; i < 4; i++ {
		other.AddTxOut(wire.NewTxOut(5000*koinuPerDoge, fromScript))
	}
	uncompressed, err := btcutil.NewWIF(key, dogeParams, false)
	require.NoError(err)
	input := map[string]any{
		"keyHex":       hex.EncodeToString(key.Serialize()),
		"vmVersions":   addressVersions(vmParams),
		"dogeVersions": addressVersions(dogeParams),
		"signers":      map[string]any{"required": signers.Required, "publicKeys": signers.PublicKeys},
		"utxo": map[string]any{
			"txid": prev.TxHash().String(), "vout": 3,
			"value": strconv.FormatInt(500*koinuPerDoge, 10), "script": hex.EncodeToString(fromScript), "confirmations": 1,
		},
		"rawTxs":          map[string]string{prev.TxHash().String(): encodeTx(prev)},
		"otherTx":         encodeTx(other),
		"uncompressedWIF": uncompressed.String(),
		"toScript":        hex.EncodeToString(signers.pkScript()),
		"amount":          120 * koinuPerDoge,
		"data":            hex.EncodeToString(encodeDestination(tagPegOut, dogeDest)),
	}
	raw, err := json.Marshal(input)
	require.NoError(err)
	inputPath := filepath.Join(t.TempDir(), "input.json")
	require.NoError(os.WriteFile(inputPath, raw, 0o600))

	out, err := exec.Command(node, "testdata/chain_vectors.mjs", inputPath).CombinedOutput()
	require.NoError(err, "%s", out)
	var got struct {
		VMAddress, DogeAddress, VMWIF, DogeWIF, KeyFromWIF, DepositAddress, TxHex string
		TxID, InflatedHex, Refused, Uncompressed                                  string
	}
	require.NoError(json.Unmarshal(out, &got), "%s", out)

	require.Equal(report.DogecoinVMAddr, got.VMAddress)
	require.Equal(report.DogecoinAddr, got.DogeAddress)
	require.Equal(report.DogecoinVMWIF, got.VMWIF)
	require.Equal(report.DogecoinWIF, got.DogeWIF)
	require.Equal(report.PrivateKeyHex, got.KeyFromWIF)
	require.Equal(wantDeposit.EncodeAddress(), got.DepositAddress)

	// The browser-signed transaction pays the reserve, carries the tag, and
	// its input verifies.
	tx, err := decodeTx(got.TxHex)
	require.NoError(err)
	require.Equal(int64(120*koinuPerDoge), tx.TxOut[0].Value)
	require.Equal(signers.pkScript(), tx.TxOut[0].PkScript)
	parsed, ok := parseDestinationTag(tx, tagPegOut)
	require.True(ok)
	require.Equal(dogeDest, parsed)

	vm, err := txscript.NewEngine(fromScript, tx, 0, txscript.StandardVerifyFlags, nil, nil,
		500*koinuPerDoge, txscript.NewCannedPrevOutputFetcher(fromScript, 500*koinuPerDoge))
	require.NoError(err)
	require.NoError(vm.Execute())

	// Change returns to the sender, and the fee is at the wallet rate.
	require.Len(tx.TxOut, 3)
	require.Equal(fromScript, tx.TxOut[2].PkScript)
	fee := int64(500*koinuPerDoge) - tx.TxOut[0].Value - tx.TxOut[2].Value
	require.Greater(fee, int64(0))
	require.LessOrEqual(fee, int64(koinuPerDoge)) // well under 1 DOGE

	require.Equal(tx.TxHash().String(), got.TxID)
	require.Equal(got.TxHex, got.InflatedHex, "an overstated UTXO value changed the transaction")
	require.Contains(got.Refused, "wrong transaction")
	require.Contains(got.Uncompressed, "uncompressed")
}

// TestEmbeddedImportsResolve checks that every relative module import in the
// embedded web files is itself embedded. go:embed skips files starting with
// _ unless the pattern uses all:, which once shipped a page that could not
// load its crypto.
func TestEmbeddedImportsResolve(t *testing.T) {
	importRE := regexp.MustCompile(`(?:from\s+|import\()\s*['"](\./[^'"]+|\.\./[^'"]+)['"]`)
	checked := 0
	err := fs.WalkDir(webFiles, "web", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !(strings.HasSuffix(p, ".js") || strings.HasSuffix(p, ".mjs")) {
			return err
		}
		src, err := fs.ReadFile(webFiles, p)
		if err != nil {
			return err
		}
		for _, m := range importRE.FindAllStringSubmatch(string(src), -1) {
			target := path.Join(path.Dir(p), m[1])
			if _, err := fs.Stat(webFiles, target); err != nil {
				t.Errorf("%s imports %s, which is not embedded", p, m[1])
			}
			checked++
		}
		return nil
	})
	require.NoError(t, err)
	require.Greater(t, checked, 5)
}

// TestPageIDsUnique checks no two elements of a page share an id: the pages
// find elements by id, and a duplicate once hid the wallet's unlock button
// behind an unrelated figure. It also checks each page marks itself, and
// only itself, as the current one in the menu.
func TestPageIDsUnique(t *testing.T) {
	pages, err := renderPages()
	require.NoError(t, err)
	require.Len(t, pages, len(sitePages))
	for path, page := range pages {
		seen := map[string]bool{}
		for _, m := range regexp.MustCompile(`\sid="([^"]+)"`).FindAllStringSubmatch(string(page), -1) {
			require.False(t, seen[m[1]], "id %q is used twice on %s", m[1], path)
			seen[m[1]] = true
		}
		current := regexp.MustCompile(`href="([^"]+)" aria-current="page"`).FindAllStringSubmatch(string(page), -1)
		require.Len(t, current, 1, path)
		require.Equal(t, path, current[0][1])
	}
	require.Greater(t, len(regexp.MustCompile(`\sid="`).FindAll(pages["/"], -1)), 50)
}

// TestWebReview runs the web wallet's review-before-signing checks.
func TestWebReview(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/review_check.mjs").CombinedOutput()
	require.NoError(t, err, string(out))
	require.Equal(t, "ok\n", string(out))
}
