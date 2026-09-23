package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/paulgnz/dogecoin-vm/btcd/btcec/v2"
	"github.com/paulgnz/dogecoin-vm/btcd/btcutil"
	"github.com/paulgnz/dogecoin-vm/btcd/chaincfg"
	"github.com/paulgnz/dogecoin-vm/btcd/txscript"
	"github.com/paulgnz/dogecoin-vm/btcd/wire"
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

// sign signs every input of tx. redeemScripts[i] is the redeem script of
// the peg output input i spends: the peg multisig or a deposit script.
func (s *signerSet) sign(tx *wire.MsgTx, redeemScripts [][]byte) error {
	if len(redeemScripts) != len(tx.TxIn) {
		return fmt.Errorf("have %d redeem scripts for %d inputs", len(redeemScripts), len(tx.TxIn))
	}

	// CHECKMULTISIG needs signatures in public key order.
	var signers []*btcec.PrivateKey
	for _, pub := range s.pubKeys {
		for _, key := range s.privKeys {
			if key.PubKey().IsEqual(pub) {
				signers = append(signers, key)
				break
			}
		}
		if len(signers) == s.Required {
			break
		}
	}
	if len(signers) < s.Required {
		return fmt.Errorf("have %d of the signer private keys, need %d", len(signers), s.Required)
	}

	for i, redeem := range redeemScripts {
		b := txscript.NewScriptBuilder().AddOp(txscript.OP_0) // CHECKMULTISIG's extra pop
		for _, key := range signers {
			sig, err := txscript.RawTxInSignature(tx, i, redeem, txscript.SigHashAll, key)
			if err != nil {
				return fmt.Errorf("signing input %d: %w", i, err)
			}
			b.AddData(sig)
		}
		sigScript, err := b.AddData(redeem).Script()
		if err != nil {
			return err
		}
		tx.TxIn[i].SignatureScript = sigScript
	}
	return nil
}
