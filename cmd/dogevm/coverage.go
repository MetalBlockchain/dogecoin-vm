package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"sort"
)

// Coverage before a cutover (Round 7, W7).
//
// A cutover covers the coins its node's wallet and registry list: a coin
// at a deposit address one node never registered, or a payment its wallet
// never saw, isn't recorded there, and a signature given before the upgrade
// could spend it beside a new one. So before the first cutover, every node
// (the coordinator and each signer) prints what it covers, and the upgrade
// compares them and stops on any difference: the same registry, and the same
// unsettled coins at the set's scripts, from every wallet's history
// (confirmed, unconfirmed, conflicted, and the unsettled file's).

// coverage is what a node would record in a cutover now.
type coverage struct {
	// Registry is a hash of the registered destinations, sorted (so file
	// formatting doesn't matter); Registered how many there are.
	Registry   string `json:"registry"`
	Registered int    `json:"registered"`
	// Coins are, per chain, the set's outputs history shows that aren't
	// yet spent or conflicted rootDepth deep, sorted.
	Coins map[string][]string `json:"coins"`
}

func nodeCoverage(b *bridge, s *pegState) (coverage, error) {
	dests, err := b.registry.list()
	if err != nil {
		return coverage{}, err
	}
	keys := make([]string, 0, len(dests))
	for _, d := range dests {
		keys = append(keys, hex.EncodeToString(d.pkScript()))
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k + "\n"))
	}
	coins, err := cutoverCoins(s, &signingLog{Actions: map[string]*loggedAction{}})
	if err != nil {
		return coverage{}, err
	}
	for chain := range coins {
		sort.Strings(coins[chain])
	}
	return coverage{Registry: hex.EncodeToString(h.Sum(nil)), Registered: len(keys), Coins: coins}, nil
}

// cmdCoverage prints this node's coverage, for comparing across the
// coordinator and every signer (deploy/upgrade-bridge.sh does):
//
//	dogevm coverage -signers FILE [-deposits FILE]
func cmdCoverage(args []string) error {
	var s settings
	fs := flag.NewFlagSet("coverage", flag.ExitOnError)
	signersPath := fs.String("signers", "", "the signer set this node runs with (public keys are enough)")
	depositsPath := fs.String("deposits", "", "this node's deposit address registry (default: deposits.json next to -signers)")
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
	c, err := nodeCoverage(b, state)
	if err != nil {
		return err
	}
	printJSON(c)
	return nil
}
