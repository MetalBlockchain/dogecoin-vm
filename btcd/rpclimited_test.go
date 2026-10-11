package btcd

import "testing"

// The public read-only RPC user (rpcLimitUser) is exposed on the websites'
// /rpc. It must not be able to broadcast, rescan, load filters or run
// whole-chain scans, and searchrawtransactions must stay bounded.
func TestLimitedUserIsReadOnly(t *testing.T) {
	for _, m := range []string{"sendrawtransaction", "rescan", "rescanblocks", "loadtxfilter",
		"getnetworkhashps", "notifyreceived", "notifyspent", "notifynewtransactions"} {
		if _, ok := rpcLimited[m]; ok {
			t.Errorf("%s is available to the limited user", m)
		}
	}
	for _, m := range []string{"getblockcount", "getblock", "getblockhash", "getrawtransaction", "searchrawtransactions", "gettxout"} {
		if _, ok := rpcLimited[m]; !ok {
			t.Errorf("%s should stay available to the limited user", m)
		}
	}
	if maxSearchRawTransactionsCount > 1000 {
		t.Errorf("searchrawtransactions cap %d is too large", maxSearchRawTransactionsCount)
	}
}

func TestSearchRawTransactionsCountIsBounded(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	tests := []struct {
		name  string
		count *int
		want  int
	}{
		{name: "default", want: 100},
		{name: "negative", count: intPointer(-1), want: 1},
		{name: "zero", count: intPointer(0), want: 0},
		{name: "below cap", count: intPointer(maxSearchRawTransactionsCount - 1), want: maxSearchRawTransactionsCount - 1},
		{name: "at cap", count: intPointer(maxSearchRawTransactionsCount), want: maxSearchRawTransactionsCount},
		{name: "above cap", count: intPointer(maxSearchRawTransactionsCount + 1), want: maxSearchRawTransactionsCount},
		{name: "maximum integer", count: &maxInt, want: maxSearchRawTransactionsCount},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := searchRawTransactionsCount(test.count); got != test.want {
				t.Fatalf("searchRawTransactionsCount() = %d, want %d", got, test.want)
			}
		})
	}
}

func intPointer(value int) *int { return &value }
