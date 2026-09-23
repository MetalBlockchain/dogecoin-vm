// Command dogevm-devnet creates a DogecoinVM subnet and chain on a local
// Metal network, paying with the pre-funded ewoq key that local networks
// allocate. It is for development only: the ewoq key is public.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/MetalBlockchain/metalgo/genesis"
	"github.com/MetalBlockchain/metalgo/ids"
	"github.com/MetalBlockchain/metalgo/vms/secp256k1fx"
	"github.com/MetalBlockchain/metalgo/wallet/subnet/primary"

	"github.com/MetalBlockchain/btcvm/vm"
)

func main() {
	uri := flag.String("uri", primary.LocalAPIURI, "node API URI")
	genesisPath := flag.String("genesis", "", "DogecoinVM genesis JSON file")
	subnetFlag := flag.String("subnet", "", "existing subnet ID (default: create one)")
	name := flag.String("name", "dogecoinvm", "chain name")
	flag.Parse()

	if *genesisPath == "" {
		log.Fatal("-genesis is required")
	}
	genesisBytes, err := os.ReadFile(*genesisPath)
	if err != nil {
		log.Fatal(err)
	}
	if !json.Valid(genesisBytes) {
		log.Fatalf("%s is not valid JSON", *genesisPath)
	}

	ctx := context.Background()
	kc := secp256k1fx.NewKeychain(genesis.EWOQKey)

	subnetID := ids.Empty
	if *subnetFlag != "" {
		if subnetID, err = ids.FromString(*subnetFlag); err != nil {
			log.Fatalf("invalid -subnet: %v", err)
		}
	} else {
		wallet, err := primary.MakePWallet(ctx, *uri, kc, primary.WalletConfig{})
		if err != nil {
			log.Fatalf("failed to sync P-chain wallet: %v", err)
		}
		tx, err := wallet.IssueCreateSubnetTx(&secp256k1fx.OutputOwners{
			Threshold: 1,
			Addrs:     []ids.ShortID{genesis.EWOQKey.Address()},
		})
		if err != nil {
			log.Fatalf("failed to create subnet: %v", err)
		}
		subnetID = tx.ID()
	}

	wallet, err := primary.MakePWallet(ctx, *uri, kc, primary.WalletConfig{
		SubnetIDs: []ids.ID{subnetID},
	})
	if err != nil {
		log.Fatalf("failed to sync P-chain wallet: %v", err)
	}
	tx, err := wallet.IssueCreateChainTx(subnetID, genesisBytes, vm.ID, nil, *name)
	if err != nil {
		log.Fatalf("failed to create chain: %v", err)
	}

	out, _ := json.Marshal(map[string]string{
		"vmID":     vm.ID.String(),
		"subnetID": subnetID.String(),
		"chainID":  tx.ID().String(),
	})
	fmt.Println(string(out))
}
