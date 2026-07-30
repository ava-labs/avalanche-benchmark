package oraclerelay

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const directStateFixture = `NETWORK=fuji
MANAGER_SUBNET_ID=2e5t2Y2xarNAfbg8yQzUwPnRcYUAB3FEBBRZo1JHuJnBqW6DGF
MANAGER_CHAIN_ID=ktKcS8mWyw4nBcYWiiPAoZ5g8XVDbU5FvzY36BCC5DfMD8zGh
MANAGER_CONVERT_TX_ID=eKnpUZDVRDcJHrV51e7ZKgD8DQq5oRmFLXkVpMcE1jbFTX3rc
SUBNET_ID=SXBGmxdaUXzKMLepDrbccpXicSdKUEjURY7pyRk9DYBu58zSY
CHAIN_ID=QWtUUC1D62ZECVugG8EJQFYo4DNe9S7wg4trpLBhKndrHc8hZ
CONVERT_TX_ID=2vnDkcHrgDavgnFRcbnvyGNTUpcv77hCnDq3H2wvHqbhVW3Y3n
MANAGER_ADDRESS=0x0000000000000000000000000000000000000001
GENESIS_EVM_ADDRESS=0x1234567890123456789012345678901234567890
FEEDER_EVM_ADDRESS=0xAbcDef0123456789abCDef0123456789ABcdEF01
ORACLE_PRICEFEED_ADDRESS=0x00000000000000000000000000000000FeedF00d
ORACLE_PRICEFEED_AGGREGATOR_ADDRESS=0x00000000000000000000000000000000FeedFacE
`

func writeState(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "network.env")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadDeploymentDirectMode(t *testing.T) {
	deployment, err := LoadDeployment(writeState(t, directStateFixture), "fuji")
	if err != nil {
		t.Fatal(err)
	}
	if deployment.HasOracle() {
		t.Fatal("deployment without ORACLE_CONVERT_TX_ID must not report an oracle")
	}
	if deployment.PriceFeedAddress.Hex() != "0x00000000000000000000000000000000FeedF00D" {
		t.Fatalf("unexpected price feed address %s", deployment.PriceFeedAddress)
	}
	if deployment.PriceFeedAggregatorAddress.Hex() != "0x00000000000000000000000000000000FEeDfAce" {
		t.Fatalf("unexpected price feed aggregator address %s", deployment.PriceFeedAggregatorAddress)
	}
	if deployment.FeederAddress.Hex() != "0xabCDeF0123456789AbcdEf0123456789aBCDEF01" {
		t.Fatalf("unexpected feeder address %s", deployment.FeederAddress)
	}
	if deployment.MainChainID.String() != "QWtUUC1D62ZECVugG8EJQFYo4DNe9S7wg4trpLBhKndrHc8hZ" {
		t.Fatalf("unexpected main chain ID %s", deployment.MainChainID)
	}
}

func TestLoadDeploymentDirectModeRequiresPriceFeed(t *testing.T) {
	withoutPriceFeed := strings.Replace(directStateFixture, "ORACLE_PRICEFEED_ADDRESS=0x00000000000000000000000000000000FeedF00d\n", "", 1)
	_, err := LoadDeployment(writeState(t, withoutPriceFeed), "fuji")
	if err == nil || !strings.Contains(err.Error(), "ORACLE_PRICEFEED_ADDRESS") {
		t.Fatalf("direct-mode state without a price feed address was accepted: %v", err)
	}
}

func TestLoadDeploymentOracleModeToleratesMissingPriceFeed(t *testing.T) {
	// Deployments created before the direct price feed existed still load for
	// the oracle-chain processes.
	oracleState := directStateFixture +
		`ORACLE_SUBNET_ID=2MHWq3eFrxhD5Z8FhXyYnzjvJVUGkekAK7KLV5q4jXCbHNqzzj
ORACLE_CHAIN_ID=2MHWq3eFrxhD5Z8FhXyYnzjvJVUGkekAK7KLV5q4jXCbHNqzzj
ORACLE_CONVERT_TX_ID=QWtUUC1D62ZECVugG8EJQFYo4DNe9S7wg4trpLBhKndrHc8hZ
ORACLE_AGGREGATOR_ADDRESS=0x000000000000000000000000000000000000FEED
ORACLE_RECEIVER_ADDRESS=0x0000000000000000000000000000000000FeedED
`
	legacy := strings.Replace(oracleState, "ORACLE_PRICEFEED_ADDRESS=0x00000000000000000000000000000000FeedF00d\n", "", 1)
	deployment, err := LoadDeployment(writeState(t, legacy), "fuji")
	if err != nil {
		t.Fatal(err)
	}
	if !deployment.HasOracle() {
		t.Fatal("oracle fields present but HasOracle is false")
	}
	if (deployment.PriceFeedAddress != [20]byte{}) {
		t.Fatalf("legacy state must leave the price feed address empty, got %s", deployment.PriceFeedAddress)
	}
}
