package main

import (
	"encoding/json"
	"flag"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConfirmationTiers(t *testing.T) {
	require := require.New(t)
	tiers, err := parseConfirmationTiers("1:1, 10:6,50:12", 20)
	require.NoError(err)
	require.Equal("1:1,10:6,50:12", formatConfirmationTiers(tiers))

	b := &bridge{depositConfirmations: 20, confirmationTiers: tiers}
	for _, tc := range []struct {
		doge string
		want int64
	}{
		{"1", 1}, {"1.00000001", 6}, {"10", 6}, {"10.5", 12}, {"50", 12}, {"50.01", 20}, {"100", 20},
	} {
		v, err := parseDoge(tc.doge)
		require.NoError(err)
		require.Equal(tc.want, b.confirmationsFor(v), tc.doge)
	}

	// No tiers: every deposit needs the full count.
	require.Equal(int64(20), (&bridge{depositConfirmations: 20}).confirmationsFor(1))
	none, err := parseConfirmationTiers("", 20)
	require.NoError(err)
	require.Nil(none)

	for _, bad := range []string{
		"10",         // no confirmations
		"10:0",       // not positive
		"10:x",       // not a number
		"10:25",      // more than the full count
		"10:6,5:3",   // amounts fall
		"10:6,50:3",  // confirmations fall
		"10:6,10:12", // amounts repeat
		"ten:6",      // not an amount
	} {
		_, err := parseConfirmationTiers(bad, 20)
		require.Error(err, bad)
	}
}

// TestPolicyWithoutTiersKeepsItsFingerprint checks a policy with no tiers
// serialises exactly as before tiers existed, so signer sets made earlier,
// and their fingerprints, stay valid.
func TestPolicyWithoutTiersKeepsItsFingerprint(t *testing.T) {
	raw, err := json.Marshal(pegPolicy{Confirmations: 20, VMFee: 1, DogeFee: 2})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "confirmationTiers")
}

// TestPolicyTiersMustAgree checks the bridge adopts the signer set's tiers,
// and refuses an explicit flag that disagrees with them.
func TestPolicyTiersMustAgree(t *testing.T) {
	require := require.New(t)
	agreed := []confirmationTier{{UpTo: koinuPerDoge, Confirmations: 1}}
	policy := &pegPolicy{Confirmations: 20, ConfirmationTiers: agreed}

	b := &bridge{depositConfirmations: 20}
	require.NoError(b.applyPolicy(policy))
	require.Equal(agreed, b.confirmationTiers)

	fs := newFlagSetForTest()
	b = bridgeFlags(fs)
	require.NoError(fs.Parse([]string{"-confirmations", "20", "-confirmation-tiers", "5:2"}))
	b.confirmationTiers, _ = parseConfirmationTiers(b.tiersFlag, 20)
	require.ErrorContains(b.applyPolicy(policy), "-confirmation-tiers")
}

func newFlagSetForTest() *flag.FlagSet { return flag.NewFlagSet("test", flag.ContinueOnError) }

// /api/info's tiers must match what confirmationsFor waits: on mainnet the
// 1-confirmation tier is raised to the floor and folded into the next.
func TestCreditTiersApplyTheMainnetFloor(t *testing.T) {
	b := &bridge{depositConfirmations: 20, confFloor: minMainnetConfirmations,
		confirmationTiers: []confirmationTier{{UpTo: 1e8, Confirmations: 1}, {UpTo: 10e8, Confirmations: 6}, {UpTo: 50e8, Confirmations: 12}}}
	got := b.creditTiers()
	want := []confirmationTier{{UpTo: 10e8, Confirmations: 6}, {UpTo: 50e8, Confirmations: 12}}
	require.Equal(t, want, got)
	for _, v := range []int64{1, 1e8, 10e8, 10e8 + 1, 50e8} {
		var n int64 = b.depositConfirmations
		for _, tr := range got {
			if v <= tr.UpTo {
				n = tr.Confirmations
				break
			}
		}
		require.Equal(t, b.confirmationsFor(v), n, "value %d", v)
	}
	b.confFloor = 0 // regtest/testnet: tiers as configured
	require.Equal(t, b.confirmationTiers, b.creditTiers())
}
