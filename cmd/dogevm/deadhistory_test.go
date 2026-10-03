package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestUnsettledTransactionsSurviveTheWallet (Round 6, V2): a transaction the
// wallet listed and that isn't settled stays on record when the wallet stops
// listing it (a full reread after a reorganisation, a rebuilt wallet) and
// across a restart; it leaves only once it is in a block rootDepth deep or
// conflicted that deep. Round 5 kept history in memory only, so a dropped
// transaction's outputs left the cutover.
func TestUnsettledTransactionsSurviveTheWallet(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	tx := h.contribute(doge, 0)
	txid := tx.TxHash().String()
	body := func(id string) (string, error) {
		require.Equal(txid, id)
		return encodeTx(tx), nil
	}
	path := filepath.Join(t.TempDir(), deadHistoryFile)
	d := &deadHistory{path: path, depth: 6}

	// Listed, unconfirmed: kept.
	gone, err := d.sync(100, map[string]historyEntry{txid: {time: 5}}, body)
	require.NoError(err)
	require.Empty(gone)

	// The wallet stops listing it: it comes back, body and all.
	gone, err = d.sync(101, map[string]historyEntry{}, body)
	require.NoError(err)
	require.Contains(gone, txid)
	back, err := decodeTx(gone[txid].Hex)
	require.NoError(err)
	require.Equal(tx.TxHash(), back.TxHash())

	// A restart: a new process reads the same file.
	d = &deadHistory{path: path, depth: 6}
	gone, err = d.sync(102, map[string]historyEntry{}, body)
	require.NoError(err)
	require.Contains(gone, txid, "a restart forgets nothing")

	// Conflicted, not yet deep: still kept, and the depth remembered.
	_, err = d.sync(103, map[string]historyEntry{txid: {conflicted: true, conflictDepth: 3}}, body)
	require.NoError(err)
	gone, err = d.sync(104, map[string]historyEntry{}, body)
	require.NoError(err)
	require.Equal(int64(3), gone[txid].ConflictDepth)

	// In a block, but shallow: kept.
	_, err = d.sync(110, map[string]historyEntry{txid: {height: 108}}, body)
	require.NoError(err)
	gone, err = d.sync(110, map[string]historyEntry{}, body)
	require.NoError(err)
	require.Contains(gone, txid)

	// Six deep: settled, and gone from the record.
	_, err = d.sync(113, map[string]historyEntry{txid: {height: 108}}, body)
	require.NoError(err)
	gone, err = d.sync(113, map[string]historyEntry{}, body)
	require.NoError(err)
	require.Empty(gone)
}

// TestDeepConflictSettlesAnUnsettledTransaction: a conflict rootDepth deep
// settles a transaction for good.
func TestDeepConflictSettlesAnUnsettledTransaction(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	tx := h.contribute(doge, 0)
	txid := tx.TxHash().String()
	body := func(string) (string, error) { return encodeTx(tx), nil }
	d := &deadHistory{path: filepath.Join(t.TempDir(), deadHistoryFile), depth: 6}
	_, err := d.sync(100, map[string]historyEntry{txid: {}}, body)
	require.NoError(err)
	_, err = d.sync(100, map[string]historyEntry{txid: {conflicted: true, conflictDepth: 6}}, body)
	require.NoError(err)
	gone, err := d.sync(100, map[string]historyEntry{}, body)
	require.NoError(err)
	require.Empty(gone)
}

// stubDogecoinCore answers the wallet calls dogeChain.history makes, from
// a list the test edits: listed[txid] = confirmations (negative:
// conflicted).
type stubDogecoinCore struct {
	listed map[string]int64
	bodies map[string]string
}

func (n *stubDogecoinCore) serve(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		var result any
		switch req.Method {
		case "listsinceblock":
			var txs []map[string]any
			for id, conf := range n.listed {
				txs = append(txs, map[string]any{"txid": id, "confirmations": conf, "time": 1})
			}
			result = map[string]any{"transactions": txs, "lastblock": "tip"}
		case "getblockheader":
			result = map[string]any{"height": 100, "confirmations": 1}
		case "getwalletinfo":
			result = map[string]any{"txcount": len(n.listed)}
		case "gettransaction":
			id := req.Params[0].(string)
			hex, ok := n.bodies[id]
			if _, listed := n.listed[id]; !ok || !listed {
				_ = json.NewEncoder(w).Encode(map[string]any{"result": nil, "error": map[string]any{"code": -5, "message": "Invalid or non-wallet transaction id"}})
				return
			}
			result = map[string]any{"hex": hex}
		default:
			t.Errorf("unexpected call %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": result, "error": nil})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestDroppedTransactionComesBackAsConflicted (V2, end to end): a
// transaction the wallet stops listing is still in dogeChain's history, as
// conflicted with no known depth, after a full reread and after a restart
// (a new client), so the cutover keeps recording its coins.
func TestDroppedTransactionComesBackAsConflicted(t *testing.T) {
	require := require.New(t)
	h := newHarness(t)
	tx := h.contribute(doge, 0)
	txid := tx.TxHash().String()
	core := &stubDogecoinCore{listed: map[string]int64{txid: 0}, bodies: map[string]string{txid: encodeTx(tx)}}
	srv := core.serve(t)
	path := filepath.Join(t.TempDir(), deadHistoryFile)
	client := func() *dogeChain {
		return &dogeChain{rpc: newRPCClient(srv.URL, "", ""), dead: &deadHistory{path: path, depth: 6}}
	}
	c := client()
	_, entries, err := c.history()
	require.NoError(err)
	require.False(entries[txid].conflicted)

	delete(core.listed, txid) // the wallet forgets it
	_, entries, err = c.history()
	require.NoError(err)
	require.True(entries[txid].conflicted, "still in history, as conflicted")
	require.Zero(entries[txid].conflictDepth, "with no depth: nothing settles it")
	got, err := c.walletTx(txid)
	require.NoError(err)
	require.Equal(tx.TxHash(), got.TxHash(), "its body from the record")

	_, entries, err = client().history() // a restart
	require.NoError(err)
	require.Contains(entries, txid)
}
