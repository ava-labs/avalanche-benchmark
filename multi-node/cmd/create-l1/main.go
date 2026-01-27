package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ava-labs/avalanchego/api/info"
	"github.com/ava-labs/avalanchego/genesis"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	"github.com/joho/godotenv"
)

var outputFile string

func main() {
	flag.StringVar(&outputFile, "output", "", "Write SUBNET_ID and CHAIN_ID to this file")
	flag.Parse()

	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// Find .env file (look in script dir, then current dir)
	envPath := findEnvFile()
	if envPath == "" {
		return fmt.Errorf(".env file not found")
	}

	if err := godotenv.Load(envPath); err != nil {
		return fmt.Errorf("failed to load .env: %w", err)
	}

	node1IP := os.Getenv("NODE1_IP")
	node2IP := os.Getenv("NODE2_IP")
	node3IP := os.Getenv("NODE3_IP")

	if node1IP == "" || node2IP == "" || node3IP == "" {
		return fmt.Errorf("missing node IPs in .env (NODE1_IP, NODE2_IP, NODE3_IP)")
	}

	nodeIPs := []string{node1IP, node2IP, node3IP}
	nodeURIs := make([]string, len(nodeIPs))
	for i, ip := range nodeIPs {
		nodeURIs[i] = fmt.Sprintf("http://%s:9650", ip)
	}

	fmt.Println("=== Create L1 ===")
	fmt.Printf("Node 1: %s\n", nodeURIs[0])
	fmt.Printf("Node 2: %s\n", nodeURIs[1])
	fmt.Printf("Node 3: %s\n", nodeURIs[2])
	fmt.Println()

	ctx := context.Background()

	// Load genesis
	genesisPath := findGenesisFile()
	if genesisPath == "" {
		return fmt.Errorf("genesis.json not found")
	}
	genesisBytes, err := os.ReadFile(genesisPath)
	if err != nil {
		return fmt.Errorf("failed to read genesis: %w", err)
	}
	fmt.Printf("Using genesis: %s\n", genesisPath)

	// Create wallet using node1
	fmt.Println("[1/4] Creating wallet...")
	kc := secp256k1fx.NewKeychain(genesis.EWOQKey)
	wallet, err := primary.MakePWallet(ctx, nodeURIs[0], kc, primary.WalletConfig{})
	if err != nil {
		return fmt.Errorf("failed to create wallet: %w", err)
	}

	// Create subnet
	fmt.Println("[2/4] Creating subnet...")
	owner := &secp256k1fx.OutputOwners{
		Threshold: 1,
		Addrs:     []ids.ShortID{genesis.EWOQKey.Address()},
	}
	subnetTx, err := wallet.IssueCreateSubnetTx(owner)
	if err != nil {
		return fmt.Errorf("failed to create subnet: %w", err)
	}
	subnetID := subnetTx.ID()
	fmt.Printf("  Subnet ID: %s\n", subnetID)

	// Re-sync wallet with subnet
	wallet, err = primary.MakePWallet(ctx, nodeURIs[0], kc, primary.WalletConfig{
		SubnetIDs: []ids.ID{subnetID},
	})
	if err != nil {
		return fmt.Errorf("failed to re-sync wallet: %w", err)
	}

	// Create chain
	fmt.Println("[3/4] Creating chain...")
	chainTx, err := wallet.IssueCreateChainTx(
		subnetID,
		genesisBytes,
		constants.SubnetEVMID,
		nil,
		"benchmarkchain",
	)
	if err != nil {
		return fmt.Errorf("failed to create chain: %w", err)
	}
	chainID := chainTx.ID()
	fmt.Printf("  Chain ID: %s\n", chainID)

	// Gather validator info from all nodes
	fmt.Println("[4/4] Converting subnet to L1...")
	fmt.Println("  Gathering validator info...")

	validators := make([]*txs.ConvertSubnetToL1Validator, 0, len(nodeURIs))
	for i, uri := range nodeURIs {
		infoClient := info.NewClient(uri)
		nodeID, nodePoP, err := infoClient.GetNodeID(ctx)
		if err != nil {
			return fmt.Errorf("failed to get node %d info: %w", i+1, err)
		}
		fmt.Printf("    Node %d: %s\n", i+1, nodeID)

		validators = append(validators, &txs.ConvertSubnetToL1Validator{
			NodeID:  nodeID.Bytes(),
			Weight:  units.Schmeckle,
			Balance: units.Avax,
			Signer:  *nodePoP,
		})
	}

	// Convert to L1
	fmt.Println("  Issuing ConvertSubnetToL1Tx...")
	_, err = wallet.IssueConvertSubnetToL1Tx(
		subnetID,
		chainID,
		[]byte{}, // Empty manager address
		validators,
	)
	if err != nil {
		return fmt.Errorf("failed to convert subnet to L1: %w", err)
	}

	// Wait for chain
	fmt.Println("  Waiting for chain to be ready...")
	time.Sleep(5 * time.Second)

	// Write output file if requested
	if outputFile != "" {
		content := fmt.Sprintf("SUBNET_ID=%s\nCHAIN_ID=%s\n", subnetID, chainID)
		if err := os.WriteFile(outputFile, []byte(content), 0644); err != nil {
			return fmt.Errorf("failed to write output file: %w", err)
		}
	}

	// Print results
	fmt.Println()
	fmt.Println("=== L1 Created Successfully ===")
	fmt.Println()
	fmt.Printf("Subnet ID: %s\n", subnetID)
	fmt.Printf("Chain ID:  %s\n", chainID)
	fmt.Println()
	fmt.Println("RPC Endpoints:")
	for i, ip := range nodeIPs {
		fmt.Printf("  Node %d: http://%s:9650/ext/bc/%s/rpc\n", i+1, ip, chainID)
	}

	return nil
}

func findEnvFile() string {
	// Check executable directory
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		envPath := filepath.Join(exeDir, "..", ".env")
		if _, err := os.Stat(envPath); err == nil {
			return envPath
		}
	}

	// Check current directory
	if _, err := os.Stat(".env"); err == nil {
		return ".env"
	}

	return ""
}

func findGenesisFile() string {
	// Check executable directory
	exe, err := os.Executable()
	if err == nil {
		exeDir := filepath.Dir(exe)
		genesisPath := filepath.Join(exeDir, "..", "genesis.json")
		if _, err := os.Stat(genesisPath); err == nil {
			return genesisPath
		}
	}

	// Check current directory
	if _, err := os.Stat("genesis.json"); err == nil {
		return "genesis.json"
	}

	return ""
}
