package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/MetalBlockchain/btcvm/btcd/btcec/v2"
	"github.com/MetalBlockchain/btcvm/btcd/btcutil"
	"github.com/MetalBlockchain/btcvm/btcd/chaincfg"
	"github.com/MetalBlockchain/btcvm/btcd/txscript"
	"github.com/MetalBlockchain/btcvm/btcd/wire"
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

	redeemScript []byte
	pubKeys      []*btcec.PublicKey
	privKeys     []*btcec.PrivateKey
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
	addr, _ := s.address(&dogecoinMainNet)
	script, _ := txscript.PayToAddrScript(addr)
	return script
}

func (s *signerSet) destination() destination {
	addr, _ := s.address(&dogecoinMainNet)
	return destination{kind: destP2SH, hash: *addr.Hash160()}
}

// sign signs every input of tx, all of which must spend peg outputs, with
// the private keys available.
func (s *signerSet) sign(tx *wire.MsgTx) error {
	if len(s.privKeys) < s.Required {
		return fmt.Errorf("have %d private keys, need %d to sign", len(s.privKeys), s.Required)
	}
	keys := map[string]*btcec.PrivateKey{}
	for _, key := range s.privKeys {
		addr, err := btcutil.NewAddressPubKey(key.PubKey().SerializeCompressed(), &dogecoinMainNet)
		if err != nil {
			return err
		}
		keys[addr.EncodeAddress()] = key
	}
	lookupKey := txscript.KeyClosure(func(addr btcutil.Address) (*btcec.PrivateKey, bool, error) {
		key, ok := keys[addr.EncodeAddress()]
		if !ok {
			return nil, false, errors.New("not a local signer")
		}
		return key, true, nil
	})
	lookupScript := txscript.ScriptClosure(func(btcutil.Address) ([]byte, error) {
		return s.redeemScript, nil
	})

	pkScript := s.pkScript()
	for i := range tx.TxIn {
		sigScript, err := txscript.SignTxOutput(&dogecoinMainNet, tx, i, pkScript,
			txscript.SigHashAll, lookupKey, lookupScript, nil)
		if err != nil {
			return fmt.Errorf("signing input %d: %w", i, err)
		}
		tx.TxIn[i].SignatureScript = sigScript
	}
	return nil
}
