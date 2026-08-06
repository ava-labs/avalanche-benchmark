// Package oraclerelay implements the price feed processes. A deployment with
// an oracle L1 runs two of them: a mock price feeder that submits prices to
// the aggregator on the oracle L1, and a control-host Warp relayer that signs
// each aggregator broadcast with the oracle validators' BLS keys and delivers
// it to the receiver on the main L1. A deployment without an oracle L1 runs
// the feeder alone: it publishes rounds directly to the Chainlink-shaped aggregator baked
// into the main chain's genesis, using type-2 (EIP-1559) transactions whose
// priority fee keeps updates ahead of benchmark flood traffic.
//
// It reuses the machinery of internal/setweight: control holds every BLS key and
// signs off-node, canonical bit positions index the P-chain validator set the
// verifier will use, and an ACP-181 epoch gate guards against a Warp validator
// view that predates the oracle conversion. Where setweight's helpers are
// unexported, the minimal logic is lifted here with a comment pointing at the
// twin; setweight.go itself is never modified.
package oraclerelay

import (
	"fmt"
	"strings"

	"github.com/ava-labs/avalanchego/ids"
	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/joho/godotenv"
)

// Deployment holds the price feed fields of deployment/network.env. The
// oracle L1 is opt-in: ORACLE_CONVERT_TX_ID missing means the deployment has
// no oracle chain and the feeder publishes directly to the main chain's
// price aggregator instead.
type Deployment struct {
	Network           string
	OracleChainID     ids.ID
	OracleSubnetID    ids.ID
	OracleConvertTxID ids.ID
	MainChainID       ids.ID
	AggregatorAddress ethcommon.Address
	ReceiverAddress   ethcommon.Address
	// PriceFeedAddress is the Chainlink-shaped proxy consumers read;
	// PriceFeedAggregatorAddress is the aggregator behind it that the direct
	// feed publishes to.
	PriceFeedAddress           ethcommon.Address
	PriceFeedAggregatorAddress ethcommon.Address
	FeederAddress              ethcommon.Address
}

// HasOracle reports whether the deployment created an oracle L1. Without one
// the only feed path is direct publication on the main chain.
func (d Deployment) HasOracle() bool {
	return d.OracleConvertTxID != ids.Empty
}

// LoadDeployment reads the price feed fields from network.env and requires
// NETWORK to match .env, mirroring weights.loadDeployment's cross-file check.
func LoadDeployment(path, network string) (Deployment, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		return Deployment{}, fmt.Errorf("read required creation state %s: %w", path, err)
	}
	if got := strings.TrimSpace(values["NETWORK"]); got != network {
		return Deployment{}, fmt.Errorf("%s: NETWORK must match .env (%q), got %q", path, network, got)
	}
	deployment := Deployment{Network: network}
	if deployment.MainChainID, err = requiredID(path, values, "CHAIN_ID"); err != nil {
		return Deployment{}, err
	}
	if deployment.FeederAddress, err = requiredAddress(path, values, "FEEDER_EVM_ADDRESS"); err != nil {
		return Deployment{}, err
	}
	hasOracle := strings.TrimSpace(values["ORACLE_CONVERT_TX_ID"]) != ""
	if hasOracle {
		if deployment.OracleConvertTxID, err = requiredID(path, values, "ORACLE_CONVERT_TX_ID"); err != nil {
			return Deployment{}, err
		}
		if deployment.OracleChainID, err = requiredID(path, values, "ORACLE_CHAIN_ID"); err != nil {
			return Deployment{}, err
		}
		if deployment.OracleSubnetID, err = requiredID(path, values, "ORACLE_SUBNET_ID"); err != nil {
			return Deployment{}, err
		}
		if deployment.AggregatorAddress, err = requiredAddress(path, values, "ORACLE_AGGREGATOR_ADDRESS"); err != nil {
			return Deployment{}, err
		}
		if deployment.ReceiverAddress, err = requiredAddress(path, values, "ORACLE_RECEIVER_ADDRESS"); err != nil {
			return Deployment{}, err
		}
	}
	// Deployments created before the direct price feed have no
	// ORACLE_PRICEFEED_* fields; that is only an error when the deployment
	// has no oracle chain, because then direct publication is the only feed
	// path.
	if raw := strings.TrimSpace(values["ORACLE_PRICEFEED_ADDRESS"]); raw != "" || !hasOracle {
		if deployment.PriceFeedAddress, err = requiredAddress(path, values, "ORACLE_PRICEFEED_ADDRESS"); err != nil {
			return Deployment{}, err
		}
	}
	if raw := strings.TrimSpace(values["ORACLE_PRICEFEED_AGGREGATOR_ADDRESS"]); raw != "" || !hasOracle {
		if deployment.PriceFeedAggregatorAddress, err = requiredAddress(path, values, "ORACLE_PRICEFEED_AGGREGATOR_ADDRESS"); err != nil {
			return Deployment{}, err
		}
	}
	return deployment, nil
}

func requiredID(path string, values map[string]string, field string) (ids.ID, error) {
	raw := strings.TrimSpace(values[field])
	if raw == "" {
		return ids.Empty, fmt.Errorf("%s: required field %s is not provided", path, field)
	}
	id, err := ids.FromString(raw)
	if err != nil {
		return ids.Empty, fmt.Errorf("%s: invalid %s %q: %w", path, field, raw, err)
	}
	return id, nil
}

func requiredAddress(path string, values map[string]string, field string) (ethcommon.Address, error) {
	raw := strings.TrimSpace(values[field])
	if raw == "" {
		return ethcommon.Address{}, fmt.Errorf("%s: required field %s is not provided", path, field)
	}
	if !ethcommon.IsHexAddress(raw) {
		return ethcommon.Address{}, fmt.Errorf("%s: field %s must be a hex address, got %q", path, field, raw)
	}
	return ethcommon.HexToAddress(raw), nil
}
