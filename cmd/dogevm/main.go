// Command dogevm is a wallet and two-way peg bridge for DogecoinVM.
//
// Connection settings come from flags or environment variables:
//
//	DOGEVM_RPC        DogecoinVM JSON-RPC URL, e.g. http://127.0.0.1:9650/ext/bc/dogecoinvm/rpc
//	DOGEVM_RPC_USER   DogecoinVM RPC user
//	DOGEVM_RPC_PASS   DogecoinVM RPC password
//	DOGEVM_NETWORK    DogecoinVM network: testnet (default) or mainnet
//	DOGECOIN_RPC      Dogecoin Core JSON-RPC URL, e.g. http://127.0.0.1:18332
//	DOGECOIN_RPC_USER Dogecoin Core RPC user
//	DOGECOIN_RPC_PASS Dogecoin Core RPC password
//	DOGECOIN_NETWORK  Dogecoin network: regtest, testnet (default) or mainnet
package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MetalBlockchain/dogecoin-vm/btcd/btcutil"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/chaincfg/chainhash"
	"github.com/MetalBlockchain/dogecoin-vm/btcd/wire"
)

const usage = `dogevm: DogecoinVM wallet and two-way peg bridge

Wallet (DogecoinVM):
  dogevm keygen                               new key, shown for both chains
  dogevm balance -address ADDR                DogecoinVM balance
  dogevm send -key KEY -to ADDR -amount DOGE  send DOGE on DogecoinVM

Peg (users):
  dogevm deposit-address -signers FILE -to VMADDR
      personal Dogecoin address that credits VMADDR; send to it from any wallet
  dogevm peg-in -signers FILE -to VMADDR -amount DOGE
      deposit from the Dogecoin Core wallet at DOGECOIN_RPC to the peg,
      credited to VMADDR on DogecoinVM
  dogevm peg-out -signers FILE -key KEY -to DOGEADDR -amount DOGE
      send DOGE from DogecoinVM back to DOGEADDR on Dogecoin

Bridge (peg signers):
  dogevm signers -required M -total N -out FILE
      create a signer set, and print the peg addresses and genesis config
  dogevm signers-check -signers FILE          check a signer set's private keys match it; print its peg addresses
  dogevm bridge -signers FILE [-once]         run the bridge
  dogevm audit -signers FILE                  check the peg is fully backed
  dogevm refund -signers FILE -list           deposits that are held or not yet credited
  dogevm refund -signers FILE -deposit TXID:VOUT [-to DOGEADDR]
      return a held deposit, less the Dogecoin fee, to its sender (or -to)
  dogevm monitor -signers FILE [-webhook URL]  alert when a health check fails
  dogevm pause -signers FILE -reason TEXT     emergency stop: sign and pay nothing (a signer: -dir DIR)
  dogevm resume -signers FILE                 end a pause
  dogevm serve -signers FILE                  web wallet and bridge API
  dogevm signer-setup STEP                    set up separate signers: init, coordinator, assemble, join, check
  dogevm signer-key -out FILE                 new key for one separate signer; prints its public key
  dogevm signer -signers FILE -key-file FILE  run one separate signer (see docs/SIGNERS.md)

Personal deposit addresses are recorded in -deposits (default deposits.json
next to the signers file) so the bridge knows to watch them.

Run "dogevm COMMAND -h" for a command's flags. Connection settings come from
the DOGEVM_* and DOGECOIN_* environment variables; see the package docs.
`

// settings holds the connection flags shared by every command.
type settings struct {
	vmRPC, vmUser, vmPass, vmNetwork     string
	dogeRPC, dogeUser, dogePass, dogeNet string
	vmParams, dogeParams                 *chaincfg.Params
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func (s *settings) register(fs *flag.FlagSet) {
	fs.StringVar(&s.vmRPC, "vm-rpc", envOr("DOGEVM_RPC", "http://127.0.0.1:9650/ext/bc/dogecoinvm/rpc"), "DogecoinVM JSON-RPC URL")
	fs.StringVar(&s.vmUser, "vm-user", os.Getenv("DOGEVM_RPC_USER"), "DogecoinVM RPC user")
	fs.StringVar(&s.vmPass, "vm-pass", os.Getenv("DOGEVM_RPC_PASS"), "DogecoinVM RPC password")
	fs.StringVar(&s.vmNetwork, "vm-network", envOr("DOGEVM_NETWORK", "testnet"), "DogecoinVM network: testnet or mainnet")
	fs.StringVar(&s.dogeRPC, "doge-rpc", envOr("DOGECOIN_RPC", "http://127.0.0.1:44555"), "Dogecoin Core JSON-RPC URL")
	fs.StringVar(&s.dogeUser, "doge-user", os.Getenv("DOGECOIN_RPC_USER"), "Dogecoin Core RPC user")
	fs.StringVar(&s.dogePass, "doge-pass", os.Getenv("DOGECOIN_RPC_PASS"), "Dogecoin Core RPC password")
	fs.StringVar(&s.dogeNet, "doge-network", envOr("DOGECOIN_NETWORK", "testnet"), "Dogecoin network: regtest, testnet or mainnet")
}

func (s *settings) resolve() error {
	var err error
	if s.vmParams, err = dogevmParams(s.vmNetwork); err != nil {
		return err
	}
	s.dogeParams, err = dogecoinParams(s.dogeNet)
	return err
}

func (s *settings) vmChain() *vmChain {
	return &vmChain{rpc: newRPCClient(s.vmRPC, s.vmUser, s.vmPass)}
}

func (s *settings) dogeRPCClient() *rpcClient {
	return newRPCClient(s.dogeRPC, s.dogeUser, s.dogePass)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	commands := map[string]func([]string) error{
		"keygen":          cmdKeygen,
		"balance":         cmdBalance,
		"send":            cmdSend,
		"peg-in":          cmdPegIn,
		"deposit-address": cmdDepositAddress,
		"serve":           cmdServe,
		"peg-out":         cmdPegOut,
		"signers":         cmdSigners,
		"bridge":          cmdBridge,
		"audit":           cmdAudit,
		"refund":          cmdRefund,
		"monitor":         cmdMonitor,
		"signer":          cmdSigner,
		"signer-key":      cmdSignerKey,
		"signer-setup":    cmdSignerSetup,
		"signers-check":   cmdSignersCheck,
		"pause":           cmdPause,
		"resume":          cmdResume,
	}
	run, ok := commands[cmd]
	if !ok {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(args); err != nil {
		fmt.Fprintln(os.Stderr, "dogevm "+cmd+":", err)
		os.Exit(1)
	}
}

func printJSON(v any) {
	out, _ := json.MarshalIndent(v, "", "  ")
	fmt.Println(string(out))
}

func parseFlags(fs *flag.FlagSet, s *settings, args []string) error {
	s.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	return s.resolve()
}

func required(values map[string]string) error {
	for name, v := range values {
		if v == "" {
			return fmt.Errorf("-%s is required", name)
		}
	}
	return nil
}

func cmdKeygen(args []string) error {
	var s settings
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	if err := parseFlags(fs, &s, args); err != nil {
		return err
	}
	key, err := newKey()
	if err != nil {
		return err
	}
	report, err := describeKey(key, s.vmParams, s.dogeParams)
	if err != nil {
		return err
	}
	printJSON(report)
	return nil
}

func cmdBalance(args []string) error {
	var s settings
	fs := flag.NewFlagSet("balance", flag.ExitOnError)
	address := fs.String("address", "", "DogecoinVM address")
	if err := parseFlags(fs, &s, args); err != nil {
		return err
	}
	if err := required(map[string]string{"address": *address}); err != nil {
		return err
	}
	addr, err := btcutil.DecodeAddress(*address, s.vmParams)
	if err != nil {
		return err
	}
	confirmed, pending, err := balance(s.vmChain(), addr)
	if err != nil {
		return err
	}
	printJSON(map[string]string{
		"address":   addr.EncodeAddress(),
		"confirmed": formatDoge(confirmed),
		"pending":   formatDoge(pending),
	})
	return nil
}

func cmdSend(args []string) error {
	var s settings
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	keyFlag := fs.String("key", "", "sender private key (WIF or hex)")
	to := fs.String("to", "", "DogecoinVM destination address")
	amountFlag := fs.String("amount", "", "amount in DOGE")
	if err := parseFlags(fs, &s, args); err != nil {
		return err
	}
	if err := required(map[string]string{"key": *keyFlag, "to": *to, "amount": *amountFlag}); err != nil {
		return err
	}
	key, err := parseKey(*keyFlag)
	if err != nil {
		return err
	}
	dest, err := btcutil.DecodeAddress(*to, s.vmParams)
	if err != nil {
		return err
	}
	amount, err := parseDoge(*amountFlag)
	if err != nil {
		return err
	}
	txid, err := payFromKey(s.vmChain(), s.vmParams, key, destinationScript(dest), amount, nil)
	if err != nil {
		return err
	}
	printJSON(map[string]string{"txid": txid.String()})
	return nil
}

func cmdPegIn(args []string) error {
	var s settings
	fs := flag.NewFlagSet("peg-in", flag.ExitOnError)
	signersPath := fs.String("signers", "", "peg signer set file")
	to := fs.String("to", "", "DogecoinVM address to credit")
	amountFlag := fs.String("amount", "", "amount in DOGE")
	if err := parseFlags(fs, &s, args); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath, "to": *to, "amount": *amountFlag}); err != nil {
		return err
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	destAddr, err := btcutil.DecodeAddress(*to, s.vmParams)
	if err != nil {
		return fmt.Errorf("-to must be a DogecoinVM %s address: %w", s.vmNetwork, err)
	}
	dest, err := destinationOf(destAddr)
	if err != nil {
		return err
	}
	amount, err := parseDoge(*amountFlag)
	if err != nil {
		return err
	}
	pegAddr, err := signers.address(s.dogeParams)
	if err != nil {
		return err
	}
	txid, err := depositFromDogecoinCore(s.dogeRPCClient(), pegAddr, dest, amount)
	if err != nil {
		return err
	}
	printJSON(map[string]string{
		"dogecoinTxid": txid,
		"pegAddress":   pegAddr.EncodeAddress(),
		"creditTo":     destAddr.EncodeAddress(),
	})
	return nil
}

func cmdPegOut(args []string) error {
	var s settings
	fs := flag.NewFlagSet("peg-out", flag.ExitOnError)
	signersPath := fs.String("signers", "", "peg signer set file")
	keyFlag := fs.String("key", "", "sender private key on DogecoinVM (WIF or hex)")
	to := fs.String("to", "", "Dogecoin address to pay")
	amountFlag := fs.String("amount", "", "amount in DOGE")
	if err := parseFlags(fs, &s, args); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath, "key": *keyFlag, "to": *to, "amount": *amountFlag}); err != nil {
		return err
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	key, err := parseKey(*keyFlag)
	if err != nil {
		return err
	}
	destAddr, err := btcutil.DecodeAddress(*to, s.dogeParams)
	if err != nil {
		return fmt.Errorf("-to must be a Dogecoin %s address: %w", s.dogeNet, err)
	}
	dest, err := destinationOf(destAddr)
	if err != nil {
		return err
	}
	amount, err := parseDoge(*amountFlag)
	if err != nil {
		return err
	}
	txid, err := payFromKey(s.vmChain(), s.vmParams, key, signers.pkScript(), amount,
		nullData(encodeDestination(tagPegOut, dest)))
	if err != nil {
		return err
	}
	printJSON(map[string]string{"dogecoinvmTxid": txid.String(), "payTo": destAddr.EncodeAddress()})
	return nil
}

func cmdSigners(args []string) error {
	var s settings
	fs := flag.NewFlagSet("signers", flag.ExitOnError)
	req := fs.Int("required", 2, "signatures required")
	total := fs.Int("total", 3, "number of signers")
	out := fs.String("out", "", "file to write the signer set to (contains private keys, unless -public-keys)")
	publicKeys := fs.String("public-keys", "", "comma-separated signer public keys (from dogevm signer-key); the set then holds no private keys")
	blocks := fs.Int("reserve-blocks", 1, "pegReserveBlocks for the genesis config (9 billion DOGE each)")
	if err := parseFlags(fs, &s, args); err != nil {
		return err
	}
	if err := required(map[string]string{"out": *out}); err != nil {
		return err
	}
	if _, err := os.Stat(*out); err == nil {
		return fmt.Errorf("%s already exists; refusing to overwrite a signer set", *out)
	}
	var signers *signerSet
	var err error
	if *publicKeys != "" {
		signers = &signerSet{Required: *req, PublicKeys: strings.Split(*publicKeys, ",")}
		err = signers.load()
	} else {
		signers, err = newSignerSet(*req, *total)
	}
	if err != nil {
		return err
	}
	if err := signers.write(*out); err != nil {
		return err
	}
	vmAddr, _ := signers.address(s.vmParams)
	dogeAddr, _ := signers.address(s.dogeParams)
	printJSON(map[string]any{
		"file":                 *out,
		"dogecoinvmReserve":    vmAddr.EncodeAddress(),
		"dogecoinPegAddress":   dogeAddr.EncodeAddress(),
		"genesisConfigSnippet": map[string]any{"pegReserveAddress": vmAddr.EncodeAddress(), "pegReserveBlocks": *blocks},
	})
	return nil
}

// cmdSignersCheck checks every private key in a signer set belongs to it, and
// prints the peg addresses the set controls. Restoring a backup uses it to
// show the restored keys are the ones holding the peg.
func cmdSignersCheck(args []string) error {
	var s settings
	fs := flag.NewFlagSet("signers-check", flag.ExitOnError)
	signersPath := fs.String("signers", "", "peg signer set file")
	if err := parseFlags(fs, &s, args); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath}); err != nil {
		return err
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	for i, key := range signers.privKeys {
		if signers.indexOf(key.PubKey()) < 0 {
			return fmt.Errorf("private key %d is not one of the set's public keys", i)
		}
	}
	vmAddr, _ := signers.address(s.vmParams)
	dogeAddr, _ := signers.address(s.dogeParams)
	printJSON(map[string]any{
		"required":           signers.Required,
		"publicKeys":         len(signers.PublicKeys),
		"privateKeys":        len(signers.privKeys),
		"dogecoinPegAddress": dogeAddr.EncodeAddress(),
		"dogecoinvmReserve":  vmAddr.EncodeAddress(),
		"fingerprint":        signers.fingerprint(),
	})
	return nil
}

// bridgeFlags registers the bridge's policy flags on fs.
func bridgeFlags(fs *flag.FlagSet) *bridge {
	b := &bridge{
		logf: func(format string, args ...any) {
			fmt.Printf("%s "+format+"\n", append([]any{time.Now().Format(time.RFC3339)}, args...)...)
		},
	}
	fs.Int64Var(&b.depositConfirmations, "confirmations", 6, "Dogecoin confirmations before a deposit is credited")
	fs.StringVar(&b.tiersFlag, "confirmation-tiers", "", `fewer confirmations for smaller deposits, as DOGE:CONFIRMATIONS pairs, e.g. "1:1,10:6,50:12"; larger deposits need -confirmations`)
	fs.Int64Var(&b.vmFee, "vm-fee", koinuPerDoge/100, "koinu deducted from each credit for the DogecoinVM fee")
	fs.Int64Var(&b.dogeFee, "doge-fee", koinuPerDoge, "koinu deducted from each peg-out for the Dogecoin fee")
	fs.Int64Var(&b.minDeposit, "min-deposit", koinuPerDoge, "smallest deposit credited, in koinu")
	fs.Int64Var(&b.minPegOut, "min-peg-out", 2*koinuPerDoge, "smallest peg-out paid, in koinu")
	fs.Int64Var(&b.maxDeposit, "max-deposit", 0, "largest deposit credited, in koinu; larger ones are held for refund (0: no cap)")
	fs.Int64Var(&b.maxCirculating, "max-circulating", 0, "most DOGE, in koinu, the bridge lets circulate on DogecoinVM (0: no cap)")
	fs.StringVar(&b.cosignersPath, "cosigners", "", "JSON list of remote signers, [{\"url\": ...}] (from dogevm signer-setup assemble)")
	fs.StringVar(&b.coordinatorKeyPath, "coordinator-key-file", "", "the coordinator key, to sign requests to the signers (from dogevm signer-setup coordinator)")
	b.flags = fs
	return b
}

// registryFor returns the deposit registry at path, or next to the signers
// file if path is empty.
func registryFor(path, signersPath string) *depositRegistry {
	if path == "" {
		path = filepath.Join(filepath.Dir(signersPath), "deposits.json")
	}
	return &depositRegistry{path: path}
}

// connect points b at the chains, the signer set and any remote signers.
func (b *bridge) connect(s *settings, signers *signerSet) error {
	b.signers = signers
	b.vm = s.vmChain()
	b.doge = &dogeChain{rpc: s.dogeRPCClient()}
	b.vmParams, b.dogeParams = s.vmParams, s.dogeParams
	if n := signers.Networks; n != nil && (n.Dogecoin != s.dogeNet || n.DogecoinVM != s.vmNetwork) {
		return fmt.Errorf("the signer set is for Dogecoin %s and DogecoinVM %s, not %s and %s",
			n.Dogecoin, n.DogecoinVM, s.dogeNet, s.vmNetwork)
	}
	tiers, err := parseConfirmationTiers(b.tiersFlag, b.depositConfirmations)
	if err != nil {
		return fmt.Errorf("-confirmation-tiers: %w", err)
	}
	b.confirmationTiers = tiers
	if err := b.applyPolicy(signers.Policy); err != nil {
		return err
	}
	if b.cosignersPath != "" {
		var err error
		if b.cosigners, err = readCosigners(b.cosignersPath); err != nil {
			return err
		}
	}
	if b.coordinatorKeyPath != "" {
		key, err := readKeyFile(b.coordinatorKeyPath)
		if err != nil {
			return err
		}
		if signers.coordKey == nil || !signers.coordKey.IsEqual(key.PubKey()) {
			return errors.New("-coordinator-key-file is not the coordinator key the signer set names")
		}
		for _, r := range b.cosigners {
			r.auth = key
		}
	}
	return nil
}

// applyPolicy adopts the policy the signers agreed to. A policy flag given
// explicitly must agree with it.
func (b *bridge) applyPolicy(p *pegPolicy) error {
	if p == nil {
		return nil
	}
	fields := map[string]*int64{
		"confirmations": &b.depositConfirmations, "vm-fee": &b.vmFee, "doge-fee": &b.dogeFee,
		"min-deposit": &b.minDeposit, "min-peg-out": &b.minPegOut,
		"max-deposit": &b.maxDeposit, "max-circulating": &b.maxCirculating,
	}
	agreed := map[string]int64{
		"confirmations": p.Confirmations, "vm-fee": p.VMFee, "doge-fee": p.DogeFee,
		"min-deposit": p.MinDeposit, "min-peg-out": p.MinPegOut,
		"max-deposit": p.MaxDeposit, "max-circulating": p.MaxCirculating,
	}
	var err error
	if b.flags != nil {
		b.flags.Visit(func(f *flag.Flag) {
			if want, ok := agreed[f.Name]; ok && *fields[f.Name] != want && err == nil {
				err = fmt.Errorf("-%s=%d disagrees with the signer set's policy (%d)", f.Name, *fields[f.Name], want)
			}
		})
	}
	for name, v := range agreed {
		*fields[name] = v
	}
	if err == nil && b.flags != nil {
		b.flags.Visit(func(f *flag.Flag) {
			if f.Name == "confirmation-tiers" && !sameTiers(b.confirmationTiers, p.ConfirmationTiers) {
				err = fmt.Errorf("-confirmation-tiers=%s disagrees with the signer set's policy (%s)",
					formatConfirmationTiers(b.confirmationTiers), formatConfirmationTiers(p.ConfirmationTiers))
			}
		})
	}
	b.confirmationTiers = p.ConfirmationTiers
	return err
}

// watchPeg makes Dogecoin Core track the peg address and every registered
// deposit address.
func watchPeg(b *bridge, rescan bool) error {
	addrs, _, err := b.dogeWatchSet(&pegState{})
	if err != nil {
		return err
	}
	for _, addr := range addrs {
		if err := b.doge.(*dogeChain).watch(addr, rescan); err != nil {
			return err
		}
	}
	return nil
}

// registerDeposit records dest's personal deposit address and has Dogecoin
// Core watch it.
func registerDeposit(b *bridge, dest destination) (btcutil.Address, error) {
	addr, err := b.signers.depositAddress(dest, b.dogeParams)
	if err != nil {
		return nil, err
	}
	added, err := b.registry.add(dest)
	if err != nil {
		return nil, err
	}
	if added {
		// Nothing can have been sent to a new address, so no rescan.
		if dc, ok := b.doge.(*dogeChain); ok {
			if err := dc.watch(addr, false); err != nil {
				return nil, err
			}
		}
		// Tell the remote signers now, so their nodes watch the address
		// before anything arrives at it. A signer that misses this learns
		// of the address from the first proposal that involves it.
		var wg sync.WaitGroup
		for _, r := range b.cosigners {
			wg.Add(1)
			go func(r *remoteSigner) {
				defer wg.Done()
				if err := r.register(dest); err != nil {
					b.logf("telling signer %s about deposit address %s: %v", r.URL, addr.EncodeAddress(), err)
				}
			}(r)
		}
		wg.Wait()
	}
	return addr, nil
}

func cmdDepositAddress(args []string) error {
	var s settings
	fs := flag.NewFlagSet("deposit-address", flag.ExitOnError)
	signersPath := fs.String("signers", "", "peg signer set file (public keys are enough)")
	depositsPath := fs.String("deposits", "", "deposit address registry (default: deposits.json next to -signers)")
	to := fs.String("to", "", "DogecoinVM address to credit")
	b := bridgeFlags(fs) // for -cosigners, to tell the signers
	if err := parseFlags(fs, &s, args); err != nil {
		return err
	}
	if err := required(map[string]string{"signers": *signersPath, "to": *to}); err != nil {
		return err
	}
	signers, err := readSignerSet(*signersPath)
	if err != nil {
		return err
	}
	b.registry = registryFor(*depositsPath, *signersPath)
	if err := b.connect(&s, signers); err != nil {
		return err
	}
	destAddr, err := btcutil.DecodeAddress(*to, s.vmParams)
	if err != nil {
		return fmt.Errorf("-to must be a DogecoinVM %s address: %w", s.vmNetwork, err)
	}
	dest, err := destinationOf(destAddr)
	if err != nil {
		return err
	}
	addr, err := registerDeposit(b, dest)
	if err != nil {
		return err
	}
	printJSON(map[string]string{
		"depositAddress": addr.EncodeAddress(),
		"creditTo":       destAddr.EncodeAddress(),
		"redeemScript":   hex.EncodeToString(signers.depositRedeemScript(dest)),
	})
	return nil
}

func cmdBridge(args []string) error {
	var s settings
	fs := flag.NewFlagSet("bridge", flag.ExitOnError)
	depositsPath := fs.String("deposits", "", "deposit address registry (default: deposits.json next to -signers)")
	signersPath := fs.String("signers", "", "peg signer set file")
	once := fs.Bool("once", false, "process what is pending, then exit")
	interval := fs.Duration("interval", 10*time.Second, "longest wait between polls; a new block on either chain starts the next one at once")
	rescan := fs.Bool("rescan", false, "rescan Dogecoin for past deposits when importing the peg address")
	s.register(fs)
	b := bridgeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
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
	if err := watchPeg(b, *rescan); err != nil {
		return fmt.Errorf("importing the peg address into Dogecoin Core: %w", err)
	}

	lastErr := ""
	for {
		for {
			done, err := b.step()
			if err != nil {
				if !*once {
					// Log a standing condition, such as a pause, once.
					if err.Error() != lastErr {
						b.logf("error: %v", err)
						lastErr = err.Error()
					}
					break
				}
				return err
			}
			if lastErr != "" {
				b.logf("running again")
				lastErr = ""
			}
			if done == "" {
				break
			}
			b.logf("%s", done)
		}
		if *once {
			return nil
		}
		b.waitForBlock(*interval)
	}
}

func cmdAudit(args []string) error {
	var s settings
	fs := flag.NewFlagSet("audit", flag.ExitOnError)
	depositsPath := fs.String("deposits", "", "deposit address registry (default: deposits.json next to -signers)")
	signersPath := fs.String("signers", "", "peg signer set file (public keys are enough)")
	s.register(fs)
	b := bridgeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
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
	if err := watchPeg(b, false); err != nil {
		return err
	}
	state, err := b.load()
	if err != nil {
		return err
	}
	a := b.audit(state)
	report := map[string]any{"solvent": a.solvent()}
	for name, v := range map[string]int64{
		"circulating": a.Circulating, "pendingPegIns": a.PendingPegIns,
		"pendingPegOuts": a.PendingPegOuts, "locked": a.Locked, "required": a.Required,
		"surplus": a.Surplus, "unclaimedOnDogecoin": a.UnclaimedOnDoge,
		"unclaimedOnDogecoinVM": a.UnclaimedOnVM,
	} {
		report[name] = formatDoge(v)
	}
	printJSON(report)
	if !a.solvent() {
		return errInsolvent
	}
	return nil
}

func parseOutPoint(s string) (wire.OutPoint, error) {
	var op wire.OutPoint
	i := strings.LastIndexByte(s, ':')
	if i < 0 {
		return op, fmt.Errorf("deposit must be TXID:VOUT, got %q", s)
	}
	hash, err := chainhash.NewHashFromStr(s[:i])
	if err != nil {
		return op, err
	}
	vout, err := strconv.ParseUint(s[i+1:], 10, 32)
	if err != nil {
		return op, err
	}
	return wire.OutPoint{Hash: *hash, Index: uint32(vout)}, nil
}

// holdReason explains why the bridge has not credited a deposit.
func (b *bridge) holdReason(d deposit) string {
	switch {
	case d.valid && d.confirmations < b.confirmationsFor(d.value):
		return fmt.Sprintf("waiting for confirmations (%d of %d)", d.confirmations, b.confirmationsFor(d.value))
	case d.valid:
		return "waiting for room under -max-circulating"
	case d.value < b.minDeposit:
		return "below the minimum deposit"
	case b.maxDeposit > 0 && d.value > b.maxDeposit:
		return "above the maximum deposit"
	default:
		return "no DogecoinVM destination"
	}
}

func cmdRefund(args []string) error {
	var s settings
	fs := flag.NewFlagSet("refund", flag.ExitOnError)
	signersPath := fs.String("signers", "", "peg signer set file")
	depositsPath := fs.String("deposits", "", "deposit address registry (default: deposits.json next to -signers)")
	list := fs.Bool("list", false, "list deposits that are held or not yet credited")
	depositFlag := fs.String("deposit", "", "deposit to refund, as TXID:VOUT")
	to := fs.String("to", "", "Dogecoin address to refund to (default: the deposit's sender)")
	force := fs.Bool("force", false, "refund a deposit the bridge may still credit (stop the bridge first)")
	s.register(fs)
	b := bridgeFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
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

	if *list {
		state, err := b.load()
		if err != nil {
			return err
		}
		rows := []map[string]string{}
		for _, d := range append(append([]deposit{}, state.held...), state.deposits...) {
			if _, done := state.released[d.outPoint]; done {
				continue
			}
			rows = append(rows, map[string]string{
				"deposit": d.outPoint.String(),
				"amount":  formatDoge(d.value),
				"status":  b.holdReason(d),
			})
		}
		printJSON(rows)
		return nil
	}

	if err := required(map[string]string{"deposit": *depositFlag}); err != nil {
		return err
	}
	op, err := parseOutPoint(*depositFlag)
	if err != nil {
		return err
	}
	var dest destination
	if *to != "" {
		addr, err := btcutil.DecodeAddress(*to, s.dogeParams)
		if err != nil {
			return fmt.Errorf("-to must be a Dogecoin %s address: %w", s.dogeNet, err)
		}
		if dest, err = destinationOf(addr); err != nil {
			return err
		}
	} else if dest, err = b.doge.(*dogeChain).sender(op.Hash); err != nil {
		return fmt.Errorf("finding the deposit's sender (pass -to): %w", err)
	}
	txid, err := b.refund(op, dest, *force)
	if err != nil {
		return err
	}
	destAddr, _ := dest.address(s.dogeParams)
	printJSON(map[string]string{"refundTxid": txid.String(), "to": destAddr.EncodeAddress()})
	return nil
}
