package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
	"github.com/stretchr/testify/require"
)

// These tests run the bridge against a real Dogecoin Core 1.14 node on
// regtest: a local dogecoind on the PATH, or, with DOGECOIND_DOCKER naming a
// Docker context (for example colima-dogetest), the dogecoind:1.14.9 image
// there (Dockerfile in docs/TESTING.md). Without either they are skipped.

type dogeNode struct {
	t        *testing.T
	rpc      *rpcClient
	url      string
	user     string
	pass     string
	restart  func()
	stopNode func()
}

func startDogecoin(t *testing.T) *dogeNode {
	t.Helper()
	var secret [16]byte
	_, _ = rand.Read(secret[:])
	n := &dogeNode{t: t, user: "test", pass: hex.EncodeToString(secret[:])}
	port := freeDogePort(t)
	n.url = fmt.Sprintf("http://127.0.0.1:%d", port)
	args := []string{"-regtest", "-server", "-listen=0", "-printtoconsole=0", "-persistmempool=0", "-txindex",
		fmt.Sprintf("-rpcport=%d", port), "-rpcuser=" + n.user, "-rpcpassword=" + n.pass}

	if ctx := os.Getenv("DOGECOIND_DOCKER"); ctx != "" {
		name := "dogevm-test-" + hex.EncodeToString(secret[:4])
		run := append([]string{"--context", ctx, "run", "-d", "--name", name,
			"-p", fmt.Sprintf("127.0.0.1:%d:%d", port, port), "dogecoind:1.14.9"},
			append(args, "-rpcbind=0.0.0.0", "-rpcallowip=0.0.0.0/0")...)
		if out, err := exec.Command("docker", run...).CombinedOutput(); err != nil {
			t.Skipf("can't start dogecoind in Docker context %s: %v: %s", ctx, err, out)
		}
		docker := func(a ...string) { _ = exec.Command("docker", append([]string{"--context", ctx}, a...)...).Run() }
		t.Cleanup(func() { docker("rm", "-f", name) })
		n.restart = func() { docker("restart", name) }
		n.stopNode = func() { docker("stop", name) }
	} else {
		bin, err := exec.LookPath("dogecoind")
		if err != nil {
			t.Skip("no dogecoind on the PATH, and DOGECOIND_DOCKER isn't set")
		}
		dir := t.TempDir()
		start := func() *exec.Cmd {
			cmd := exec.Command(bin, append(args, "-datadir="+dir)...)
			require.NoError(t, cmd.Start())
			return cmd
		}
		cmd := start()
		t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
		n.restart = func() { _ = cmd.Process.Kill(); _ = cmd.Wait(); cmd = start() }
	}
	n.rpc = newRPCClient(n.url, n.user, n.pass)
	n.waitReady()
	n.mine(150) // mature coinbases to spend (regtest maturity is 60)
	return n
}

func freeDogePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func (n *dogeNode) waitReady() {
	n.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		var count int64
		if err := n.rpc.call(&count, "getblockcount"); err == nil {
			return
		}
		require.True(n.t, time.Now().Before(deadline), "dogecoind didn't start")
		time.Sleep(300 * time.Millisecond)
	}
}

func (n *dogeNode) mine(blocks int) {
	n.t.Helper()
	require.NoError(n.t, n.rpc.call(nil, "generate", blocks))
}

func (n *dogeNode) inMempool(txid chainhash.Hash) bool {
	var ids []string
	require.NoError(n.t, n.rpc.call(&ids, "getrawmempool"))
	for _, id := range ids {
		if id == txid.String() {
			return true
		}
	}
	return false
}

// pay sends value DOGE to address, with an OP_RETURN of data (hex) if any.
func (n *dogeNode) pay(address string, value float64, data string) string {
	n.t.Helper()
	outputs := map[string]any{address: value}
	if data != "" {
		outputs["data"] = data
	}
	var unsigned string
	require.NoError(n.t, n.rpc.call(&unsigned, "createrawtransaction", []any{}, outputs))
	var funded struct {
		Hex string `json:"hex"`
	}
	require.NoError(n.t, n.rpc.call(&funded, "fundrawtransaction", unsigned))
	var signed struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	require.NoError(n.t, n.rpc.call(&signed, "signrawtransaction", funded.Hex))
	require.True(n.t, signed.Complete)
	var txid string
	require.NoError(n.t, n.rpc.call(&txid, "sendrawtransaction", signed.Hex))
	return txid
}

func (n *dogeNode) bridgeHarness() *harness {
	n.t.Helper()
	h := newBareHarness(n.t)
	h.b.doge = &dogeChain{rpc: n.rpc}
	h.b.dogeParams = &dogecoinRegTest
	require.NoError(n.t, watchPeg(h.b, false))
	// The peg's first coin, its anchor: a contribution.
	peg, err := h.b.signers.address(h.b.dogeParams)
	require.NoError(n.t, err)
	n.pay(peg.EncodeAddress(), 10, hex.EncodeToString(tagContrib))
	n.mine(6)
	return h
}

// TestBridgeWithDogecoinCore: a deposit to a personal address is credited,
// a withdrawal is paid by a transaction Dogecoin Core relays (version 1,
// BIP125, change to the peg), a payout stuck past -bump-after is replaced
// by one Dogecoin Core accepts in its place, and the peg stays solvent.
func TestBridgeWithDogecoinCore(t *testing.T) {
	require := require.New(t)
	n := startDogecoin(t)
	h := n.bridgeHarness()

	alice := h.user(1)
	depositAddr, err := registerDeposit(h.b, alice)
	require.NoError(err)
	n.pay(depositAddr.EncodeAddress(), 50, "")
	require.Empty(h.step(), "unconfirmed: not credited")
	n.mine(6)
	require.NotEmpty(h.step())
	require.Equal(50*doge-h.b.vmFee, paidTo(h.vm, alice))
	h.vm.mine()

	var bobAddr string
	require.NoError(n.rpc.call(&bobAddr, "getnewaddress"))
	decoded, err := btcutil.DecodeAddress(bobAddr, h.b.dogeParams)
	require.NoError(err)
	bob, err := destinationOf(decoded)
	require.NoError(err)
	h.pegOut(20*doge, bob)
	did, err := h.b.step()
	require.NoError(err)
	require.Contains(did, "paid")
	s, err := h.b.load()
	require.NoError(err)
	require.Len(s.unconfirmed, 1)
	var first chainhash.Hash
	for id := range s.unconfirmed {
		first = id
	}
	require.True(n.inMempool(first), "Dogecoin Core relayed the payout")
	payout := s.unconfirmed[first].tx
	require.Equal(int32(1), payout.Version)
	require.Equal(h.b.signers.pkScript(), payout.TxOut[1].PkScript, "change back to the peg")

	// It waits past -bump-after: replaced, and Dogecoin Core takes the
	// replacement in its place.
	h.b.bumpAfter = time.Second
	time.Sleep(2 * time.Second)
	did, err = h.b.step()
	require.NoError(err)
	require.Contains(did, "replaced "+first.String())
	require.False(n.inMempool(first), "the first payout left the mempool")
	s, err = h.b.load()
	require.NoError(err)
	var second chainhash.Hash
	for id := range s.unconfirmed {
		if id != first {
			second = id
		}
	}
	require.True(n.inMempool(second), "the replacement is in the mempool")
	h.b.bumpAfter = time.Hour
	n.mine(1)
	require.Empty(h.step())

	a := h.audit()
	require.True(a.solvent(), "%+v", a)
	require.Zero(a.PendingPegOuts)
}

// TestEvictedPayoutIsResent: a payout the node drops from its mempool (a
// restart without a saved mempool) is handed back to it.
func TestEvictedPayoutIsResent(t *testing.T) {
	require := require.New(t)
	n := startDogecoin(t)
	h := n.bridgeHarness()
	h.b.bumpAfter = 0
	alice := h.user(1)
	depositAddr, err := registerDeposit(h.b, alice)
	require.NoError(err)
	n.pay(depositAddr.EncodeAddress(), 50, "")
	n.mine(6)
	require.NotEmpty(h.step())
	h.vm.mine()
	h.pegOut(20*doge, h.user(9))
	require.Contains(h.step(), "paid")
	s, err := h.b.load()
	require.NoError(err)
	var txid chainhash.Hash
	for id := range s.unconfirmed {
		txid = id
	}

	n.restart()
	n.waitReady()
	h.b.resent = nil // as after its own restart
	_, err = h.b.step()
	require.NoError(err)
	require.Eventually(func() bool { return n.inMempool(txid) }, 30*time.Second, 500*time.Millisecond,
		"the payout is back in the mempool")
}

// TestWalletHistoryAcrossAReorg: the incremental history read follows a
// reorganisation that takes a credited-looking deposit out of its block.
func TestWalletHistoryAcrossAReorg(t *testing.T) {
	require := require.New(t)
	n := startDogecoin(t)
	h := n.bridgeHarness()
	alice := h.user(1)
	depositAddr, err := registerDeposit(h.b, alice)
	require.NoError(err)
	txid := n.pay(depositAddr.EncodeAddress(), 50, "")
	n.mine(1)
	s, err := h.b.load()
	require.NoError(err)
	confs := func(s *pegState) int64 {
		for _, d := range s.deposits {
			if d.outPoint.Hash.String() == txid {
				return d.confirmations
			}
		}
		return -1
	}
	require.Equal(int64(1), confs(s))

	var best string
	require.NoError(n.rpc.call(&best, "getbestblockhash"))
	require.NoError(n.rpc.call(nil, "invalidateblock", best))
	s, err = h.b.load()
	require.NoError(err)
	require.Equal(int64(0), confs(s), "out of its block: unconfirmed again")

	require.NoError(n.rpc.call(nil, "reconsiderblock", best))
	n.mine(2)
	s, err = h.b.load()
	require.NoError(err)
	require.Equal(int64(3), confs(s))
}

// TestLineAndSignedIDsOnDogecoinCore: on a real node, the peg's line moves
// to a payout's change once it is in a block, a signer follows it from its
// anchor, and the signer finds the payout by the ID Dogecoin Core lists it
// under (P2SH signatures change it), and still finds it by that ID with
// its wallet's listing gone (round 3, S1 and S5).
func TestLineAndSignedIDsOnDogecoinCore(t *testing.T) {
	require := require.New(t)
	n := startDogecoin(t)
	h := n.bridgeHarness()
	full := h.b.signers
	own := *h.b
	own.signers = publicOnly(t, full)
	log, err := createSigningLog(t.TempDir() + "/signing-log.json")
	require.NoError(err)
	c := &cosigner{b: &own, key: full.privKeys[0], log: log}
	setAnchors(t, c)

	alice := h.user(1)
	depositAddr, err := registerDeposit(h.b, alice)
	require.NoError(err)
	n.pay(depositAddr.EncodeAddress(), 50, "")
	n.mine(6)
	require.NotEmpty(h.step())
	h.vm.mine()
	var bobAddr string
	require.NoError(n.rpc.call(&bobAddr, "getnewaddress"))
	decoded, err := btcutil.DecodeAddress(bobAddr, h.b.dogeParams)
	require.NoError(err)
	bob, err := destinationOf(decoded)
	require.NoError(err)
	req := h.pegOut(20*doge, bob)
	require.Contains(h.step(), "paid")
	s, err := h.b.load()
	require.NoError(err)
	var payout *wire.MsgTx
	for _, u := range s.unconfirmed {
		payout = u.tx
	}
	require.NotNil(payout)
	// As if c had signed it: its log holds it without signatures.
	key, err := (action{Kind: actionPayout, PegOut: req.TxHash().String()}).key()
	require.NoError(err)
	require.NoError(c.log.record(key, chainDogecoin, unsignedCopy(payout), 20*doge, 0))
	n.mine(1)

	s, err = c.b.load()
	require.NoError(err)
	nodes := func(string) chain { return c.b.doge }
	require.NoError(c.learnSigned(newChainEvidence(s, nodes)))
	entry := c.log.Actions[key].Txs[0]
	require.Equal([]string{payout.TxHash().String()}, entry.Signed)
	require.NotEqual(entry.Txid, payout.TxHash().String())
	blind := &chainEvidence{listed: map[string]map[string]chainTx{}, nodes: nodes}
	require.True(blind.confirmed(chainDogecoin, entry), "the node finds it by the ID it lists")
	require.False(blind.confirmed(chainDogecoin, loggedTx{Txid: entry.Txid}), "but not by the unsigned one")

	change := wire.OutPoint{Hash: payout.TxHash(), Index: 1}
	got, err := tips(s)
	require.NoError(err)
	require.Equal(change.String(), got[chainDogecoin], "the line moved to the payout's change")
	probe := wire.NewMsgTx(1)
	probe.AddTxIn(wire.NewTxIn(&change, nil, nil))
	require.NoError(c.log.anchorMoved(chainDogecoin, probe, unspentOn(c.b.doge), s.pegLine), "the signer follows it")
}

// unsignedCopy is tx without its signatures, as a signer signed it.
func unsignedCopy(tx *wire.MsgTx) *wire.MsgTx {
	c := tx.Copy()
	for _, in := range c.TxIn {
		in.SignatureScript = nil
	}
	return c
}

// TestRefundAfterCancelOnDogecoinCore: a deposit over the maximum that
// names a destination is cancelled on DogecoinVM first, then refunded by a
// transaction Dogecoin Core relays and mines (round 3, S2).
func TestRefundAfterCancelOnDogecoinCore(t *testing.T) {
	require := require.New(t)
	n := startDogecoin(t)
	h := n.bridgeHarness()
	h.b.maxDeposit = 40 * doge
	alice := h.user(1)
	depositAddr, err := registerDeposit(h.b, alice)
	require.NoError(err)
	txid := n.pay(depositAddr.EncodeAddress(), 50, "")
	n.mine(6)
	require.Empty(h.step(), "over the maximum: held")
	hash, err := chainhash.NewHashFromStr(txid)
	require.NoError(err)
	s, err := h.b.load()
	require.NoError(err)
	var op wire.OutPoint
	for _, d := range s.held {
		if d.outPoint.Hash == *hash {
			op = d.outPoint
		}
	}
	require.Equal(*hash, op.Hash)
	var backAddr string
	require.NoError(n.rpc.call(&backAddr, "getnewaddress"))
	decoded, err := btcutil.DecodeAddress(backAddr, h.b.dogeParams)
	require.NoError(err)
	back, err := destinationOf(decoded)
	require.NoError(err)

	_, err = h.b.refund(op, back, false)
	require.ErrorIs(err, errCancelSent)
	h.vm.mine()
	refund, err := h.b.refund(op, back, false)
	require.NoError(err)
	require.True(n.inMempool(refund), "Dogecoin Core relayed the refund")
	n.mine(1)
	a := h.audit()
	require.True(a.solvent(), "%+v", a)
	require.Zero(a.UnclaimedOnDoge)
}
