// Package oraclerelay implements the two oracle-chain processes: a mock price
// feeder that submits prices to the aggregator on the oracle L1, and a
// control-host Warp relayer that signs each aggregator broadcast with the oracle
// validators' BLS keys and delivers it to the receiver on the main L1.
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

// Deployment holds the oracle-specific fields of deployment/network.env. The
// oracle L1 is opt-in, so ORACLE_CONVERT_TX_ID missing means the deployment has
// no oracle and both commands must fail before doing any work.
type Deployment struct {
	Network           string
	OracleChainID     ids.ID
	OracleSubnetID    ids.ID
	OracleConvertTxID ids.ID
	MainChainID       ids.ID
	AggregatorAddress ethcommon.Address
	ReceiverAddress   ethcommon.Address
	FeederAddress     ethcommon.Address
}

// LoadDeployment reads the oracle fields from network.env and requires NETWORK
// to match .env, mirroring weights.loadDeployment's cross-file check.
func LoadDeployment(path, network string) (Deployment, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		return Deployment{}, fmt.Errorf("read required creation state %s: %w", path, err)
	}
	if got := strings.TrimSpace(values["NETWORK"]); got != network {
		return Deployment{}, fmt.Errorf("%s: NETWORK must match .env (%q), got %q", path, network, got)
	}
	if strings.TrimSpace(values["ORACLE_CONVERT_TX_ID"]) == "" {
		return Deployment{}, fmt.Errorf("%s: this deployment has no oracle L1 (ORACLE_CONVERT_TX_ID is not provided)", path)
	}
	deployment := Deployment{Network: network}
	if deployment.OracleConvertTxID, err = requiredID(path, values, "ORACLE_CONVERT_TX_ID"); err != nil {
		return Deployment{}, err
	}
	if deployment.OracleChainID, err = requiredID(path, values, "ORACLE_CHAIN_ID"); err != nil {
		return Deployment{}, err
	}
	if deployment.OracleSubnetID, err = requiredID(path, values, "ORACLE_SUBNET_ID"); err != nil {
		return Deployment{}, err
	}
	if deployment.MainChainID, err = requiredID(path, values, "CHAIN_ID"); err != nil {
		return Deployment{}, err
	}
	if deployment.AggregatorAddress, err = requiredAddress(path, values, "ORACLE_AGGREGATOR_ADDRESS"); err != nil {
		return Deployment{}, err
	}
	if deployment.ReceiverAddress, err = requiredAddress(path, values, "ORACLE_RECEIVER_ADDRESS"); err != nil {
		return Deployment{}, err
	}
	if deployment.FeederAddress, err = requiredAddress(path, values, "FEEDER_EVM_ADDRESS"); err != nil {
		return Deployment{}, err
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
