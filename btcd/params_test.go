package btcd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/blockchain"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil/hdkeychain"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// TestDogecoinVMGenesisHashes pins the genesis blocks. Changing either one
// starts a new chain, so it must be a deliberate edit to this test.
func TestDogecoinVMGenesisHashes(t *testing.T) {
	tests := []struct {
		params *chaincfg.Params
		want   string
	}{
		{&DogecoinVMMainNetParams, "930a12968573c7205b456a54b8f0fe21c9d7598b846aceafc39bb8423e92a468"},
		{&DogecoinVMTestNetParams, "f769f49347df638a23e6340e8c809ff680d92bbde6c47523c1de215618129859"},
	}
	for _, test := range tests {
		if got := test.params.GenesisHash.String(); got != test.want {
			t.Errorf("%s: genesis hash %s, want %s", test.params.Name, got, test.want)
		}
		if got := test.params.GenesisBlock.BlockHash(); got != *test.params.GenesisHash {
			t.Errorf("%s: GenesisHash does not match GenesisBlock", test.params.Name)
		}
		for _, out := range test.params.GenesisBlock.Transactions[0].TxOut {
			if out.Value != 0 {
				t.Errorf("%s: genesis output pays %d, want no premine", test.params.Name, out.Value)
			}
		}
	}
}

// TestDogecoinVMEncodings checks that addresses, WIF keys and extended keys
// use Dogecoin's encodings.
func TestDogecoinVMEncodings(t *testing.T) {
	tests := []struct {
		params           *chaincfg.Params
		p2pkh, p2sh, wif string
		extPriv, extPub  string
	}{
		{&DogecoinVMMainNetParams, "D", "9A", "Q", "dgpv", "dgub"},
		{&DogecoinVMTestNetParams, "n", "2", "c", "tprv", "tpub"},
	}

	hash160 := bytes.Repeat([]byte{0x42}, 20)
	privKey, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{0x01}, 32))
	seed := bytes.Repeat([]byte{0x07}, hdkeychain.RecommendedSeedLen)

	for _, test := range tests {
		name := test.params.Name

		pkh, err := btcutil.NewAddressPubKeyHash(hash160, test.params)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "P2PKH", pkh.EncodeAddress(), test.p2pkh)
		decoded, err := btcutil.DecodeAddress(pkh.EncodeAddress(), test.params)
		if err != nil || !decoded.IsForNet(test.params) {
			t.Errorf("%s: P2PKH address does not round-trip: %v", name, err)
		}

		sh, err := btcutil.NewAddressScriptHashFromHash(hash160, test.params)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "P2SH", sh.EncodeAddress(), test.p2sh)

		wif, err := btcutil.NewWIF(privKey, test.params, true)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "WIF", wif.String(), test.wif)

		master, err := hdkeychain.NewMaster(seed, test.params)
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "extended private key", master.String(), test.extPriv)
		pub, err := master.Neuter()
		if err != nil {
			t.Fatal(err)
		}
		checkPrefix(t, name, "extended public key", pub.String(), test.extPub)
	}
}

func checkPrefix(t *testing.T, network, kind, encoded, allowed string) {
	t.Helper()
	// A single-character spec lists alternative first characters; a longer
	// one is a literal prefix.
	if len(allowed) > 1 && strings.ToLower(allowed) == allowed {
		if !strings.HasPrefix(encoded, allowed) {
			t.Errorf("%s: %s %s does not start with %q", network, kind, encoded, allowed)
		}
		return
	}
	if !strings.ContainsRune(allowed, rune(encoded[0])) {
		t.Errorf("%s: %s %s does not start with one of %q", network, kind, encoded, allowed)
	}
}

// TestDogecoinVMNoBlockSubsidy checks that DogecoinVM never mints DOGE through
// the coinbase.
func TestDogecoinVMNoBlockSubsidy(t *testing.T) {
	for _, params := range []*chaincfg.Params{&DogecoinVMMainNetParams, &DogecoinVMTestNetParams} {
		for _, height := range []int32{0, 1, 100000, 210000, 1 << 30} {
			if got := blockchain.CalcBlockSubsidy(height, params); got != 0 {
				t.Errorf("%s: subsidy at height %d is %d, want 0", params.Name, height, got)
			}
		}
	}
}

// TestDogecoinMaxMoney checks that single outputs are bounded by Dogecoin's
// MAX_MONEY of 10 billion DOGE rather than Bitcoin's 21 million.
func TestDogecoinMaxMoney(t *testing.T) {
	const koinuPerDoge = 1e8

	tests := []struct {
		name  string
		value int64
		valid bool
	}{
		{"21M DOGE, over Bitcoin's cap", 21e6*koinuPerDoge + 1, true},
		{"1B DOGE", 1e9 * koinuPerDoge, true},
		{"exactly MAX_MONEY", 10e9 * koinuPerDoge, true},
		{"one koinu over MAX_MONEY", 10e9*koinuPerDoge + 1, false},
	}
	for _, test := range tests {
		tx := wire.NewMsgTx(1)
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 0}, nil, nil))
		tx.AddTxOut(wire.NewTxOut(test.value, []byte{0x51}))

		err := blockchain.CheckTransactionSanity(btcutil.NewTx(tx))
		if (err == nil) != test.valid {
			t.Errorf("%s: CheckTransactionSanity error = %v, want valid=%v", test.name, err, test.valid)
		}
	}
}
