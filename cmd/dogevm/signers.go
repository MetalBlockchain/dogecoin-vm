package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcec/v2/ecdsa"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// signerSet is the m-of-n multisig that holds the peg: DOGE locked on
// Dogecoin and the reserve on DogecoinVM are both locked to the same redeem
// script, so they share one P2SH hash on both chains.
//
// A signers file holds public keys and, for development, the private keys
// too. In production each signer keeps its own key and signs separately.
type signerSet struct {
	Required   int      `json:"required"`
	PublicKeys []string `json:"publicKeys"`
	// PrivateKeys holds hex private keys this process may sign with.
	PrivateKeys []string `json:"privateKeys,omitempty"`

	// Set by a signer ceremony (dogevm signer-setup); absent in older sets.
	// Networks, Operators, CoordinatorKey and Policy are what every signer
	// agrees to, and the fingerprint covers them.
	Networks       *setNetworks   `json:"networks,omitempty"`
	Operators      []operatorCard `json:"operators,omitempty"`
	CoordinatorKey string         `json:"coordinatorKey,omitempty"` // hex; signs every request to the signers
	Policy         *pegPolicy     `json:"policy,omitempty"`

	path         string // the file it was read from, if any
	redeemScript []byte
	pubKeys      []*btcec.PublicKey
	privKeys     []*btcec.PrivateKey
	coordKey     *btcec.PublicKey
}

type setNetworks struct {
	Dogecoin   string `json:"dogecoin"`
	DogecoinVM string `json:"dogecoinvm"`
}

// pegPolicy is the bridge's policy, in koinu. Signers rebuild transactions
// with it, so coordinator and signers must agree on it exactly.
type pegPolicy struct {
	Confirmations  int64 `json:"confirmations"`
	VMFee          int64 `json:"vmFee"`
	DogeFee        int64 `json:"dogeFee"`
	MinDeposit     int64 `json:"minDeposit"`
	MinPegOut      int64 `json:"minPegOut"`
	MaxDeposit     int64 `json:"maxDeposit"`
	MaxCirculating int64 `json:"maxCirculating"`
	// Fewer confirmations for smaller deposits; omitted when there are
	// none, so sets made before tiers keep their fingerprint.
	ConfirmationTiers []confirmationTier `json:"confirmationTiers,omitempty"`
}

// fingerprint identifies everything the signers agree to. Each signer reads
// it out to the others over a separate channel before joining.
func (s *signerSet) fingerprint() string {
	agreed, _ := json.Marshal(struct {
		Required       int            `json:"required"`
		PublicKeys     []string       `json:"publicKeys"`
		Networks       *setNetworks   `json:"networks"`
		Operators      []operatorCard `json:"operators"`
		CoordinatorKey string         `json:"coordinatorKey"`
		Policy         *pegPolicy     `json:"policy"`
	}{s.Required, s.PublicKeys, s.Networks, s.Operators, s.CoordinatorKey, s.Policy})
	sum := sha256.Sum256(agreed)
	h := hex.EncodeToString(sum[:10])
	return strings.Join([]string{h[0:4], h[4:8], h[8:12], h[12:16], h[16:20]}, "-")
}

// publicCopy is the set without private keys.
func (s *signerSet) publicCopy() *signerSet {
	c := *s
	c.PrivateKeys, c.privKeys = nil, nil
	return &c
}

func newSignerSet(required, total int) (*signerSet, error) {
	if required < 1 || required > total || total > 15 {
		return nil, fmt.Errorf("need 1 <= required (%d) <= total (%d) <= 15", required, total)
	}
	s := &signerSet{Required: required}
	for i := 0; i < total; i++ {
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return nil, err
		}
		key, _ := btcec.PrivKeyFromBytes(secret[:])
		s.PublicKeys = append(s.PublicKeys, hex.EncodeToString(key.PubKey().SerializeCompressed()))
		s.PrivateKeys = append(s.PrivateKeys, hex.EncodeToString(secret[:]))
	}
	return s, s.load()
}

func readSignerSet(path string) (*signerSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s signerSet
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	s.path = path
	return &s, s.load()
}

func (s *signerSet) write(path string) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o600)
}

// load parses the keys and builds the redeem script.
func (s *signerSet) load() error {
	var addrs []*btcutil.AddressPubKey
	for _, h := range s.PublicKeys {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return fmt.Errorf("public key %s: %w", h, err)
		}
		pub, err := btcec.ParsePubKey(raw)
		if err != nil {
			return fmt.Errorf("public key %s: %w", h, err)
		}
		s.pubKeys = append(s.pubKeys, pub)
		// The network only affects encoding, which the script does not use.
		addr, err := btcutil.NewAddressPubKey(pub.SerializeCompressed(), &dogecoinMainNet)
		if err != nil {
			return err
		}
		addrs = append(addrs, addr)
	}
	for _, h := range s.PrivateKeys {
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != 32 {
			return errors.New("private keys must be 32-byte hex")
		}
		key, _ := btcec.PrivKeyFromBytes(raw)
		s.privKeys = append(s.privKeys, key)
	}
	if s.CoordinatorKey != "" {
		raw, err := hex.DecodeString(s.CoordinatorKey)
		if err != nil {
			return fmt.Errorf("coordinator key: %w", err)
		}
		if s.coordKey, err = btcec.ParsePubKey(raw); err != nil {
			return fmt.Errorf("coordinator key: %w", err)
		}
	}
	if s.Required < 1 || s.Required > len(addrs) {
		return fmt.Errorf("required signatures %d out of range for %d keys", s.Required, len(addrs))
	}

	var err error
	s.redeemScript, err = txscript.MultiSigScript(addrs, s.Required)
	return err
}

// address returns the peg P2SH address on the network params encode.
func (s *signerSet) address(params *chaincfg.Params) (*btcutil.AddressScriptHash, error) {
	return btcutil.NewAddressScriptHash(s.redeemScript, params)
}

func (s *signerSet) pkScript() []byte {
	return p2shScript(s.redeemScript)
}

func (s *signerSet) destination() destination {
	return destination{kind: destP2SH, hash: hash160Of(s.redeemScript)}
}

// depositRedeemScript is the redeem script of dest's personal deposit
// address: <kind || hash160> OP_DROP followed by the peg multisig. Only the
// signers can spend it, exactly as with the peg address, but each DogecoinVM
// address gets its own Dogecoin address, so deposits need no OP_RETURN and
// any wallet can make them.
func (s *signerSet) depositRedeemScript(dest destination) []byte {
	script := make([]byte, 0, 2+21+len(s.redeemScript))
	script = append(script, txscript.OP_DATA_21, dest.kind)
	script = append(script, dest.hash[:]...)
	script = append(script, txscript.OP_DROP)
	return append(script, s.redeemScript...)
}

func (s *signerSet) depositAddress(dest destination, params *chaincfg.Params) (*btcutil.AddressScriptHash, error) {
	return btcutil.NewAddressScriptHash(s.depositRedeemScript(dest), params)
}

func hash160Of(script []byte) [20]byte {
	var h [20]byte
	copy(h[:], btcutil.Hash160(script))
	return h
}

func p2shScript(redeemScript []byte) []byte {
	return destination{kind: destP2SH, hash: hash160Of(redeemScript)}.pkScript()
}

// sign signs every input of tx with the private keys this process holds.
// redeemScripts[i] is the redeem script of the peg output input i spends:
// the peg multisig or a deposit script.
func (s *signerSet) sign(tx *wire.MsgTx, redeemScripts [][]byte) error {
	sigs := map[int][][]byte{}
	for i, pub := range s.pubKeys {
		for _, key := range s.privKeys {
			if key.PubKey().IsEqual(pub) {
				inputSigs, err := signInputs(tx, redeemScripts, key)
				if err != nil {
					return err
				}
				sigs[i] = inputSigs
				break
			}
		}
	}
	return s.assemble(tx, redeemScripts, sigs)
}

// indexOf returns the position of pub in the set, or -1.
func (s *signerSet) indexOf(pub *btcec.PublicKey) int {
	for i, p := range s.pubKeys {
		if p.IsEqual(pub) {
			return i
		}
	}
	return -1
}

// signInputs returns key's signature, with its sighash byte, for each input
// of tx. redeemScripts[i] is the script input i spends.
func signInputs(tx *wire.MsgTx, redeemScripts [][]byte, key *btcec.PrivateKey) ([][]byte, error) {
	if len(redeemScripts) != len(tx.TxIn) {
		return nil, fmt.Errorf("have %d redeem scripts for %d inputs", len(redeemScripts), len(tx.TxIn))
	}
	sigs := make([][]byte, len(tx.TxIn))
	for i, redeem := range redeemScripts {
		sig, err := txscript.RawTxInSignature(tx, i, redeem, txscript.SigHashAll, key)
		if err != nil {
			return nil, fmt.Errorf("signing input %d: %w", i, err)
		}
		sigs[i] = sig
	}
	return sigs, nil
}

// verifyInputs checks sigs are the signer at index's signatures of every
// input of tx.
func (s *signerSet) verifyInputs(tx *wire.MsgTx, redeemScripts [][]byte, index int, sigs [][]byte) error {
	if index < 0 || index >= len(s.pubKeys) {
		return fmt.Errorf("no signer %d", index)
	}
	if len(sigs) != len(tx.TxIn) || len(redeemScripts) != len(tx.TxIn) {
		return fmt.Errorf("have %d signatures for %d inputs", len(sigs), len(tx.TxIn))
	}
	for i, sig := range sigs {
		if len(sig) < 2 || txscript.SigHashType(sig[len(sig)-1]) != txscript.SigHashAll {
			return fmt.Errorf("input %d: not a SIGHASH_ALL signature", i)
		}
		parsed, err := ecdsa.ParseDERSignature(sig[:len(sig)-1])
		if err != nil {
			return fmt.Errorf("input %d: %w", i, err)
		}
		hash, err := txscript.CalcSignatureHash(redeemScripts[i], txscript.SigHashAll, tx, i)
		if err != nil {
			return err
		}
		if !parsed.Verify(hash, s.pubKeys[index]) {
			return fmt.Errorf("input %d: signature does not verify", i)
		}
	}
	return nil
}

// assemble completes tx's signature scripts from the signatures of at least
// Required signers, keyed by their position in the set.
func (s *signerSet) assemble(tx *wire.MsgTx, redeemScripts [][]byte, sigs map[int][][]byte) error {
	if len(redeemScripts) != len(tx.TxIn) {
		return fmt.Errorf("have %d redeem scripts for %d inputs", len(redeemScripts), len(tx.TxIn))
	}
	// CHECKMULTISIG needs signatures in public key order.
	var signers []int
	for i := range s.pubKeys {
		if _, ok := sigs[i]; ok && len(signers) < s.Required {
			signers = append(signers, i)
		}
	}
	if len(signers) < s.Required {
		return fmt.Errorf("have signatures from %d signers, need %d", len(signers), s.Required)
	}
	for i, redeem := range redeemScripts {
		b := txscript.NewScriptBuilder().AddOp(txscript.OP_0) // CHECKMULTISIG's extra pop
		for _, signer := range signers {
			b.AddData(sigs[signer][i])
		}
		sigScript, err := b.AddData(redeem).Script()
		if err != nil {
			return err
		}
		tx.TxIn[i].SignatureScript = sigScript
	}
	return nil
}
