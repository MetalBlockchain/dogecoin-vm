package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/txscript"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

// Peg messages are carried in a transaction's single OP_RETURN output:
//
//	DVMD type hash160      Dogecoin deposit to the peg; credit this VM address
//	DVMI txid vout         VM release crediting that Dogecoin deposit
//	DVMO type hash160      VM peg-out to the reserve; pay this Dogecoin address
//	DVMR txid              Dogecoin payment for that VM peg-out
//	DVMF txid vout         Dogecoin refund of a deposit that was not credited
//
// type is 0 for P2PKH and 1 for P2SH. txids are in internal byte order and
// vout is little endian.
var (
	tagDeposit = []byte("DVMD")
	tagRelease = []byte("DVMI")
	tagPegOut  = []byte("DVMO")
	tagPayment = []byte("DVMR")
	tagRefund  = []byte("DVMF")
)

const (
	destP2PKH byte = 0
	destP2SH  byte = 1
)

// destination is a pay-to-hash address in a network-neutral form: the same
// key or script hash on Dogecoin and on DogecoinVM.
type destination struct {
	kind byte
	hash [20]byte
}

func destinationOf(addr btcutil.Address) (destination, error) {
	var d destination
	switch a := addr.(type) {
	case *btcutil.AddressPubKeyHash:
		d.kind, d.hash = destP2PKH, *a.Hash160()
	case *btcutil.AddressScriptHash:
		d.kind, d.hash = destP2SH, *a.Hash160()
	default:
		return d, fmt.Errorf("unsupported address type %T: use a P2PKH or P2SH address", addr)
	}
	return d, nil
}

// address encodes d for params.
func (d destination) address(params *chaincfg.Params) (btcutil.Address, error) {
	if d.kind == destP2SH {
		return btcutil.NewAddressScriptHashFromHash(d.hash[:], params)
	}
	return btcutil.NewAddressPubKeyHash(d.hash[:], params)
}

func (d destination) pkScript() []byte {
	if d.kind == destP2SH {
		script, _ := txscript.NewScriptBuilder().AddOp(txscript.OP_HASH160).
			AddData(d.hash[:]).AddOp(txscript.OP_EQUAL).Script()
		return script
	}
	script, _ := txscript.NewScriptBuilder().AddOp(txscript.OP_DUP).AddOp(txscript.OP_HASH160).
		AddData(d.hash[:]).AddOp(txscript.OP_EQUALVERIFY).AddOp(txscript.OP_CHECKSIG).Script()
	return script
}

// destinationOfScript reads a P2PKH or P2SH output script.
func destinationOfScript(script []byte) (destination, error) {
	var d destination
	switch txscript.GetScriptClass(script) {
	case txscript.PubKeyHashTy:
		d.kind = destP2PKH
		copy(d.hash[:], script[3:23])
	case txscript.ScriptHashTy:
		d.kind = destP2SH
		copy(d.hash[:], script[2:22])
	default:
		return d, errors.New("not a P2PKH or P2SH output")
	}
	return d, nil
}

func encodeDestination(tag []byte, d destination) []byte {
	return append(append(append([]byte{}, tag...), d.kind), d.hash[:]...)
}

func decodeDestination(payload []byte) (destination, error) {
	var d destination
	if len(payload) != 1+20 || payload[0] > destP2SH {
		return d, errors.New("malformed destination")
	}
	d.kind = payload[0]
	copy(d.hash[:], payload[1:])
	return d, nil
}

func encodeRelease(deposit wire.OutPoint) []byte {
	return encodeOutPointTag(tagRelease, deposit)
}

func encodeRefund(deposit wire.OutPoint) []byte {
	return encodeOutPointTag(tagRefund, deposit)
}

func encodeOutPointTag(tag []byte, op wire.OutPoint) []byte {
	out := append(append([]byte{}, tag...), op.Hash[:]...)
	return binary.LittleEndian.AppendUint32(out, op.Index)
}

func encodePayment(request chainhash.Hash) []byte {
	return append(append([]byte{}, tagPayment...), request[:]...)
}

// opReturnData returns the tag and payload of tx's OP_RETURN output, if it
// has exactly one.
func opReturnData(tx *wire.MsgTx) (tag, payload []byte, ok bool) {
	var data []byte
	found := 0
	for _, out := range tx.TxOut {
		if txscript.GetScriptClass(out.PkScript) != txscript.NullDataTy {
			continue
		}
		found++
		pushes, err := txscript.PushedData(out.PkScript)
		if err != nil {
			return nil, nil, false
		}
		data = bytes.Join(pushes, nil)
	}
	if found != 1 || len(data) < 4 {
		return nil, nil, false
	}
	return data[:4], data[4:], true
}

func nullData(data []byte) *wire.TxOut {
	script, err := txscript.NullDataScript(data)
	if err != nil {
		panic(err) // data is always far below the size limit
	}
	return wire.NewTxOut(0, script)
}

// parseRelease reads the deposit a VM release credits.
func parseRelease(tx *wire.MsgTx) (wire.OutPoint, bool) {
	return parseOutPointTag(tx, tagRelease)
}

// parseRefund reads the deposit a Dogecoin refund returns.
func parseRefund(tx *wire.MsgTx) (wire.OutPoint, bool) {
	return parseOutPointTag(tx, tagRefund)
}

func parseOutPointTag(tx *wire.MsgTx, want []byte) (wire.OutPoint, bool) {
	tag, payload, ok := opReturnData(tx)
	if !ok || !bytes.Equal(tag, want) || len(payload) != 36 {
		return wire.OutPoint{}, false
	}
	var op wire.OutPoint
	copy(op.Hash[:], payload[:32])
	op.Index = binary.LittleEndian.Uint32(payload[32:])
	return op, true
}

// parsePayment reads the VM peg-out a Dogecoin payment pays.
func parsePayment(tx *wire.MsgTx) (chainhash.Hash, bool) {
	tag, payload, ok := opReturnData(tx)
	if !ok || !bytes.Equal(tag, tagPayment) || len(payload) != 32 {
		return chainhash.Hash{}, false
	}
	var h chainhash.Hash
	copy(h[:], payload)
	return h, true
}

// parseDestinationTag reads a DVMD or DVMO destination.
func parseDestinationTag(tx *wire.MsgTx, want []byte) (destination, bool) {
	tag, payload, ok := opReturnData(tx)
	if !ok || !bytes.Equal(tag, want) {
		return destination{}, false
	}
	d, err := decodeDestination(payload)
	return d, err == nil
}
