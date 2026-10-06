package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// Inventory before a cutover (Round 8, X5).
//
// `dogevm audit` prints totals. Before the first cutover the operator needs
// the coins themselves: every coin of the set on both chains with its value
// and depth, how many fences they take and what those cost, the surplus that
// has to cover it (each signer promises at most half the surplus, so it must
// be twice the fees), and whatever would stop a safe adoption: a coin not
// yet in a block, a coin shallower than rootDepth, or a transaction of the
// set the wallet sees conflicted short of rootDepth (its outputs could still
// come back). `-require-ready` makes any of those an error, which is how
// deploy/upgrade-bridge.sh uses it before adopting anything.

// inventoryCoin is one coin of the set.
type inventoryCoin struct {
	OutPoint      string `json:"outpoint"`
	Value         string `json:"value"`
	Confirmations int64  `json:"confirmations"`
}

// inventoryChain is one chain's coins and fences.
type inventoryChain struct {
	Coins []inventoryCoin `json:"coins"`
	Total string          `json:"total"`
	// Batches is how many fences spend every coin; FenceFees what they pay
	// together; Headroom what replacements of a stuck one may add.
	Batches   int    `json:"batches"`
	FenceFees string `json:"fenceFees"`
	Headroom  string `json:"headroom"`
}

// inventory is a node's view of the set before a cutover.
type inventory struct {
	RootDepth int64 `json:"rootDepth"`
	// Addresses are the set's Dogecoin addresses: the peg address and every
	// registered personal deposit address, to reconcile against a full node
	// or explorer outpoint by outpoint before adopting (RUNBOOK.md).
	Addresses []string                  `json:"addresses"`
	Chains    map[string]inventoryChain `json:"chains"`
	// Conflicted are transactions of the set the wallet sees conflicted
	// short of rootDepth.
	Conflicted []string `json:"conflicted"`
	Surplus    string   `json:"surplus"`
	// Needed is twice the fences' fees and headroom: each signer promises
	// at most half the surplus (feeRoom).
	Needed   string   `json:"needed"`
	Problems []string `json:"problems"`
	Ready    bool     `json:"ready"`
}

// fenceReplacements is the headroom kept for replacing a stuck Dogecoin
// fence: one more fence fee each time.
const fenceReplacements = 4

func nodeInventory(b *bridge, s *pegState) (inventory, error) {
	inv := inventory{RootDepth: s.view(chainDogecoin).depth, Chains: map[string]inventoryChain{}, Conflicted: []string{}, Problems: []string{}, Addresses: []string{}}
	for script := range s.redeemFor {
		if _, addrs, _, err := txscript.ExtractPkScriptAddrs([]byte(script), b.dogeParams); err == nil && len(addrs) == 1 {
			inv.Addresses = append(inv.Addresses, addrs[0].EncodeAddress())
		}
	}
	sort.Strings(inv.Addresses)
	var needed int64
	for _, chain := range cutoverChains {
		var coins []utxo
		switch chain {
		case chainDogecoin:
			coins = append(coins, s.lockedUTXOs...)
		case chainDogecoinVM:
			coins = append(coins, s.reserveUTXOs...)
		}
		sort.Slice(coins, func(i, j int) bool { return outPointLess(coins[i].outPoint, coins[j].outPoint) })
		ic := inventoryChain{Coins: []inventoryCoin{}}
		var total int64
		var confirmed []utxo
		deep := int64(1)
		if chain == chainDogecoin {
			deep = inv.RootDepth
		}
		for _, u := range coins {
			ic.Coins = append(ic.Coins, inventoryCoin{OutPoint: u.outPoint.String(), Value: formatDoge(u.value), Confirmations: u.confirmations})
			total += u.value
			switch {
			case u.confirmations <= 0:
				inv.Problems = append(inv.Problems, fmt.Sprintf("%s: %v is not in a block", chain, u.outPoint))
			case u.confirmations < deep:
				inv.Problems = append(inv.Problems, fmt.Sprintf("%s: %v has %d of %d confirmations", chain, u.outPoint, u.confirmations, deep))
			}
			if u.confirmations > 0 {
				confirmed = append(confirmed, u)
			}
		}
		// A payment to the set that isn't in a block yet (either chain):
		// wait for it, or for what replaces it, before adopting (Round 9,
		// Y2). Its coin isn't in the list above (that holds only coins the
		// wallet lists as unspent in a block).
		v := s.view(chain)
		for _, t := range v.txs {
			if t.confirmations > 0 {
				continue
			}
			for _, out := range t.tx.TxOut {
				if v.isCoin(out) {
					inv.Problems = append(inv.Problems, fmt.Sprintf("%s: %v pays the set and is not in a block yet", chain, t.tx.TxHash()))
					break
				}
			}
		}
		ic.Total = formatDoge(total)
		fees, batches, costliest, infeasible, err := b.fenceBudget(s, chain)
		if err != nil {
			return inventory{}, err
		}
		if infeasible != "" {
			inv.Problems = append(inv.Problems, infeasible)
		}
		ic.Batches, ic.FenceFees = batches, formatDoge(fees)
		// A stuck Dogecoin fence is replaced one fence fee higher each time
		// (its own size-based fee), one batch at a time (Round 9, Y4).
		var headroom int64
		if chain == chainDogecoin {
			headroom = int64(fenceReplacements) * costliest
		}
		ic.Headroom = formatDoge(headroom)
		needed += fees + headroom
		inv.Chains[chain] = ic
	}
	v := s.view(chainDogecoin)
	for _, t := range s.dogeConflicted {
		if hash := t.tx.TxHash(); v.conflictDepth[hash] < v.depth {
			inv.Conflicted = append(inv.Conflicted, hash.String())
			inv.Problems = append(inv.Problems, fmt.Sprintf("dogecoin: %v is conflicted only %d deep; its outputs could still come back", hash, v.conflictDepth[hash]))
		}
	}
	sort.Strings(inv.Conflicted)
	a := b.audit(s)
	inv.Surplus, inv.Needed = formatDoge(a.Surplus), formatDoge(2*needed)
	if a.Surplus < 2*needed {
		inv.Problems = append(inv.Problems, fmt.Sprintf("the surplus of %s DOGE is less than the %s DOGE the fences need (twice their fees and headroom: each signer promises at most half); contribute (DVMC) first",
			formatDoge(a.Surplus), formatDoge(2*needed)))
	}
	inv.Ready = len(inv.Problems) == 0
	return inv, nil
}

// fenceBudget is what the fences on chain will cost, how many there will
// be, and the costliest one, worked out by building them (Round 10, Z4):
// the coins in fenceOrder's order (the lineage's tip first, then oldest
// first), each batch as nextFence makes it (at most maxFenceInputs,
// trimmed to maxFenceBytes, each input's own redeem script sized), built
// with buildFence, and each batch after the first starting with the real
// change the one before paid back. A batch that can't pay its fee and
// leave pegDust is a problem, not a number: infeasible says why.
func (b *bridge) fenceBudget(s *pegState, chain string) (fees int64, batches int, costliest int64, infeasible string, err error) {
	coins, err := s.fenceOrder(chain)
	if err != nil {
		return 0, 0, 0, err.Error(), nil
	}
	var carry *utxo
	for len(coins) > 0 {
		var batch []utxo
		if carry != nil {
			batch = append(batch, *carry)
		}
		take := min(len(coins), maxFenceInputs(chain)-len(batch))
		batch = append(batch, coins[:take]...)
		if batch, err = b.trimToSize(s, chain, batch); err != nil {
			return 0, 0, 0, "", err
		}
		used := len(batch)
		if carry != nil {
			used-- // the carried change isn't one of coins
		}
		if used <= 0 {
			return fees, batches, costliest, fmt.Sprintf("%s: a fence can't take another coin past the change it carries", chain), nil
		}
		// The carried change sits at the set's own script, as the anchor
		// before it did: size it like that one.
		size, err := b.fenceSizeOf(s, chain, batch)
		if err != nil {
			return 0, 0, 0, "", err
		}
		fee := fenceFee(chain, b.fenceBase(chain), size)
		tx, err := b.buildFence(chain, batch, fee)
		if err != nil {
			return fees, batches, costliest, fmt.Sprintf("%s: fence %d: %v", chain, batches+1, err), nil
		}
		fees += fee
		costliest = max(costliest, fee)
		batches++
		coins = coins[used:]
		carry = &utxo{outPoint: wire.OutPoint{Hash: tx.TxHash(), Index: 0}, value: tx.TxOut[0].Value,
			pkScript: tx.TxOut[0].PkScript, confirmations: 1}
	}
	return fees, batches, costliest, "", nil
}

// cmdInventory prints this node's inventory:
//
//	dogevm inventory -signers FILE [-deposits FILE] [-require-ready]
func cmdInventory(args []string) error {
	var s settings
	fs := flag.NewFlagSet("inventory", flag.ExitOnError)
	signersPath := fs.String("signers", "", "the signer set this node runs with (public keys are enough)")
	depositsPath := fs.String("deposits", "", "this node's deposit address registry (default: deposits.json next to -signers)")
	ready := fs.Bool("require-ready", false, "exit with an error if anything would stop a safe adoption")
	s.register(fs)
	b := bridgeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	named := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { named[f.Name] = true })
	if !named["doge-network"] && os.Getenv("DOGECOIN_NETWORK") == "" || !named["vm-network"] && os.Getenv("DOGEVM_NETWORK") == "" {
		return errors.New("name the networks: -doge-network and -vm-network, or DOGECOIN_NETWORK and DOGEVM_NETWORK")
	}
	if err := s.resolve(); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath}); err != nil {
		return err
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	if err := b.connect(&s, signers); err != nil {
		return err
	}
	b.registry = registryFor(*depositsPath, *signersPath)
	b.unsettledPath = unsettledFor(*signersPath)
	b.contributionsPath = filepath.Join(filepath.Dir(*signersPath), "contributions.txt")
	if err := watchPeg(b, false); err != nil {
		return err
	}
	state, err := b.load()
	if err != nil {
		return err
	}
	inv, err := nodeInventory(b, state)
	if err != nil {
		return err
	}
	printJSON(inv)
	if *ready && !inv.Ready {
		return fmt.Errorf("not ready to adopt: %d problem(s), listed above", len(inv.Problems))
	}
	return nil
}
