package main

import (
	"fmt"

	btcd "github.com/MetalBlockchain/dogecoin-vm/btcd"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg"
)

const koinuPerDoge = 1e8

// Dogecoin network encodings, from Dogecoin Core's chainparams.cpp. Only the
// fields address and key encoding use are set.
var (
	dogecoinMainNet = chaincfg.Params{Name: "mainnet", PubKeyHashAddrID: 30, ScriptHashAddrID: 22, PrivateKeyID: 158}
	dogecoinTestNet = chaincfg.Params{Name: "testnet", PubKeyHashAddrID: 113, ScriptHashAddrID: 196, PrivateKeyID: 241}
	dogecoinRegTest = chaincfg.Params{Name: "regtest", PubKeyHashAddrID: 111, ScriptHashAddrID: 196, PrivateKeyID: 239}
)

func dogecoinParams(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet":
		return &dogecoinMainNet, nil
	case "testnet":
		return &dogecoinTestNet, nil
	case "regtest":
		return &dogecoinRegTest, nil
	}
	return nil, fmt.Errorf("unknown Dogecoin network %q (mainnet, testnet or regtest)", network)
}

func dogevmParams(network string) (*chaincfg.Params, error) {
	switch network {
	case "mainnet":
		return &btcd.DogecoinVMMainNetParams, nil
	case "testnet":
		return &btcd.DogecoinVMTestNetParams, nil
	}
	return nil, fmt.Errorf("unknown DogecoinVM network %q (mainnet or testnet)", network)
}

// formatDoge renders koinu as a DOGE decimal string without floating point.
func formatDoge(koinu int64) string {
	sign := ""
	if koinu < 0 {
		sign, koinu = "-", -koinu
	}
	return fmt.Sprintf("%s%d.%08d", sign, koinu/koinuPerDoge, koinu%koinuPerDoge)
}

// parseDoge parses a DOGE decimal string into koinu without floating point.
func parseDoge(s string) (int64, error) {
	var whole, frac int64
	var fracDigits int
	seenDot := false
	if s == "" {
		return 0, fmt.Errorf("empty amount")
	}
	for _, c := range s {
		switch {
		case c == '.' && !seenDot:
			seenDot = true
		case c >= '0' && c <= '9' && !seenDot:
			whole = whole*10 + int64(c-'0')
			if whole > 10_000_000_000 {
				return 0, fmt.Errorf("amount %s is too large", s)
			}
		case c >= '0' && c <= '9' && fracDigits < 8:
			frac = frac*10 + int64(c-'0')
			fracDigits++
		default:
			return 0, fmt.Errorf("invalid amount %q", s)
		}
	}
	for ; fracDigits < 8; fracDigits++ {
		frac *= 10
	}
	return whole*koinuPerDoge + frac, nil
}
