package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// fixedSetJSON is a signer set as signer-setup assemble wrote it before
// transport keys: its fingerprint must never change, or live signers would
// refuse the set they run with.
const fixedSetJSON = `{
  "required": 2,
  "publicKeys": [
    "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798",
    "02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5",
    "02f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9"
  ],
  "networks": {"dogecoin": "mainnet", "dogecoinvm": "mainnet"},
  "operators": [
    {"name": "Signer 1", "url": "http://127.0.0.1:9701", "publicKey": "0279be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798", "proof": "00"},
    {"name": "Signer 2", "url": "http://127.0.0.1:9702", "publicKey": "02c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5", "proof": "00"},
    {"name": "Signer 3", "url": "http://127.0.0.1:9703", "publicKey": "02f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9", "proof": "00"}
  ],
  "coordinatorKey": "02e493dbf1c10d80f3581e4904930b1404cc6c13900ee0758474fa94abe8c4cd13",
  "policy": {"confirmations": 20, "vmFee": 1000000, "dogeFee": 10000000, "minDeposit": 10000,
    "minPegOut": 30000, "maxDeposit": 100000000, "maxCirculating": 1000000000,
    "confirmationTiers": [{"upTo": 1000000, "confirmations": 1}]}
}`

func fixedSet(t *testing.T) *signerSet {
	var s signerSet
	require.NoError(t, json.Unmarshal([]byte(fixedSetJSON), &s))
	require.NoError(t, s.load())
	return &s
}

// TestFingerprintOfExistingSetsIsStable: fields added to the set since are
// left out of the fingerprint when empty.
func TestFingerprintOfExistingSetsIsStable(t *testing.T) {
	require.Equal(t, "6680-1ad4-9dce-4088-51d7", fixedSet(t).fingerprint())
}
