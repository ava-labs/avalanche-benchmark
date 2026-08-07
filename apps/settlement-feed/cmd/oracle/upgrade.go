package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ava-labs/avalanche-benchmark/apps/settlement-feed/internal/oraclerelay"
	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanche-benchmark/internal/creation"
	ethcommon "github.com/ava-labs/libevm/common"
)

// defaultActivationMinutes leaves room to distribute the file and restart a
// full fleet one node at a time before the upgrade activates. subnet-evm
// refuses an upgrade whose timestamp is already in the past when a node
// loads it.
const defaultActivationMinutes = 15

// upgradeCommand renders ./upgrade.json: the direct price feed's accounts as
// a subnet-evm stateUpgrades entry, rendered for this deployment's feeder
// key. This is THE install path for the app: genesis stays base-layer-only,
// so installing an app never forces a chain re-creation. Apply the fragment
// with `fleet upgrade upgrade.json`.
func upgradeCommand(root, minutesArgument string) error {
	minutes := defaultActivationMinutes
	if minutesArgument != "" {
		parsed, err := strconv.Atoi(minutesArgument)
		if err != nil || parsed < 1 {
			return fmt.Errorf("activation delay must be a positive number of minutes, got %q", minutesArgument)
		}
		minutes = parsed
	}

	public, _, err := creation.LoadPublic(filepath.Join(root, "deployment", "public.json"))
	if err != nil {
		return err
	}
	if !ethcommon.IsHexAddress(public.FeederAddress) {
		return fmt.Errorf("deployment/public.json has no valid feeder address, got %q", public.FeederAddress)
	}
	feeder := ethcommon.HexToAddress(public.FeederAddress)

	allocations := creation.DirectFeedAllocations(feeder)
	withReceiver := false
	// The oracle-L1 shape adds the main chain's Warp receiver. Its trust
	// anchor is the oracle chain ID, which exists only after `l1 create`
	// recorded it, so the receiver installs through this fragment and never
	// through genesis.
	if public.HasOracle() {
		environment, err := config.LoadNetworkEnvironment(filepath.Join(root, ".env"))
		if err != nil {
			return err
		}
		deployment, err := oraclerelay.LoadDeployment(filepath.Join(root, "deployment", "network.env"), environment.Network)
		if err != nil {
			return err
		}
		if !deployment.HasOracle() {
			return fmt.Errorf("the inventory declares an oracle chain but deployment/network.env has no oracle record; run l1 create first")
		}
		allocations = append(allocations, creation.OracleReceiverAllocation(deployment.OracleChainID))
		withReceiver = true
	}

	accounts := make(map[string]map[string]any)
	for _, allocation := range allocations {
		if allocation.RuntimeCode == "" || allocation.RuntimeCode == "0x" {
			return fmt.Errorf("contract %s has empty runtime code; refusing to render", allocation.Address.Hex())
		}
		storage := make(map[string]string, len(allocation.Storage))
		for slot, value := range allocation.Storage {
			// An explicit zero in upgrade.json passes the first restart and
			// then bricks the node: the database reads the zero back as
			// absent and subnet-evm's deep-equal check refuses the config.
			if value == (ethcommon.Hash{}) {
				return fmt.Errorf("contract %s seeds slot %s with zero; explicit zeros are forbidden in upgrade.json", allocation.Address.Hex(), slot.Hex())
			}
			storage[slot.Hex()] = value.Hex()
		}
		accounts[allocation.Address.Hex()] = map[string]any{
			"code":    allocation.RuntimeCode,
			"storage": storage,
		}
	}

	activation := time.Now().Add(time.Duration(minutes) * time.Minute).Unix()
	upgrade := map[string]any{
		"stateUpgrades": []any{
			map[string]any{
				"blockTimestamp": activation,
				"accounts":       accounts,
			},
		},
	}
	contents, err := json.MarshalIndent(upgrade, "", "  ")
	if err != nil {
		return err
	}

	target := filepath.Join(root, "upgrade.json")
	if _, err := os.Stat(target); err == nil {
		return fmt.Errorf("./upgrade.json already exists; move or delete it explicitly")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect ./upgrade.json: %w", err)
	}
	if err := os.WriteFile(target, append(contents, '\n'), 0o600); err != nil {
		return err
	}
	rendered := "direct feed accounts"
	if withReceiver {
		rendered = "direct feed accounts + the main-chain Warp receiver"
	}
	fmt.Printf("wrote ./upgrade.json: %s, activation %s (in %d minutes)\n",
		rendered, time.Unix(activation, 0).UTC().Format(time.RFC3339), minutes)
	fmt.Println("apply it with: fleet upgrade upgrade.json")
	fmt.Println("every node must restart with the file BEFORE the activation time")
	return nil
}
