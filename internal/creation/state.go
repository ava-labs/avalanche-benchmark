package creation

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanchego/ids"
)

// ChainRecord holds the creation output of one L1 beyond the legacy main and
// oracle chains, which keep their named State fields.
type ChainRecord struct {
	SubnetID    ids.ID
	ChainID     ids.ID
	ConvertTxID ids.ID
}

type State struct {
	Path               string
	Network            string
	ManagerSubnetID    ids.ID
	ManagerChainID     ids.ID
	ManagerConvertTxID ids.ID
	OracleSubnetID     ids.ID
	OracleChainID      ids.ID
	OracleConvertTxID  ids.ID
	SubnetID           ids.ID
	ChainID            ids.ID
	ConvertTxID        ids.ID
	// Chains records every additional chain by name. Main and oracle stay in
	// the named fields above so old deployments load unchanged.
	Chains                     map[string]ChainRecord
	ManagerAddress             string
	GenesisEVMAddress          string
	FeederEVMAddress           string
	OracleAggregatorAddress    string
	OracleReceiverAddress      string
	PriceFeedAddress           string
	PriceFeedAggregatorAddress string
}

// ChainKey turns a chain name into its network.env key fragment: "trading"
// becomes TRADING, "risk-2" becomes RISK_2. The mapping is injective because
// chain names cannot contain underscores.
func ChainKey(name string) string {
	return strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// StateChainKeys returns the network.env field names recording a chain's
// subnet ID, chain ID, and convert transaction ID. Main keeps the bare
// legacy keys and oracle its ORACLE_ prefix, so old deployments load
// unchanged; every other chain gets keys derived from its name.
func StateChainKeys(chain string) (subnetKey, chainIDKey, convertKey string) {
	switch chain {
	case config.MainChain:
		return "SUBNET_ID", "CHAIN_ID", "CONVERT_TX_ID"
	case config.OracleChain:
		return "ORACLE_SUBNET_ID", "ORACLE_CHAIN_ID", "ORACLE_CONVERT_TX_ID"
	default:
		key := ChainKey(chain)
		return "SUBNET_" + key + "_ID", "CHAIN_" + key + "_ID", "CONVERT_" + key + "_TX_ID"
	}
}

// ChainIDsFromState reads one chain's IDs out of parsed network.env values.
// Absent or invalid values come back as an error naming the missing field, so
// callers can distinguish "not created yet" from "created" per chain.
func ChainIDsFromState(values map[string]string, chain string) (chainID, subnetID ids.ID, err error) {
	subnetKey, chainIDKey, _ := StateChainKeys(chain)
	parse := func(field string) (ids.ID, error) {
		value := strings.TrimSpace(values[field])
		if value == "" {
			return ids.Empty, fmt.Errorf("required field %s is not provided", field)
		}
		id, err := ids.FromString(value)
		if err != nil {
			return ids.Empty, fmt.Errorf("invalid %s: %w", field, err)
		}
		return id, nil
	}
	if chainID, err = parse(chainIDKey); err != nil {
		return ids.Empty, ids.Empty, err
	}
	if subnetID, err = parse(subnetKey); err != nil {
		return ids.Empty, ids.Empty, err
	}
	return chainID, subnetID, nil
}

// chainNames returns the recorded extra chain names in stable order.
func (s State) chainNames() []string {
	names := make([]string, 0, len(s.Chains))
	for name := range s.Chains {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s State) Save() error {
	fields := []struct {
		key   string
		value string
	}{
		{"NETWORK", s.Network},
		{"MANAGER_SUBNET_ID", idString(s.ManagerSubnetID)},
		{"MANAGER_CHAIN_ID", idString(s.ManagerChainID)},
		{"MANAGER_CONVERT_TX_ID", idString(s.ManagerConvertTxID)},
		{"ORACLE_SUBNET_ID", idString(s.OracleSubnetID)},
		{"ORACLE_CHAIN_ID", idString(s.OracleChainID)},
		{"ORACLE_CONVERT_TX_ID", idString(s.OracleConvertTxID)},
		{"SUBNET_ID", idString(s.SubnetID)},
		{"CHAIN_ID", idString(s.ChainID)},
		{"CONVERT_TX_ID", idString(s.ConvertTxID)},
	}
	for _, name := range s.chainNames() {
		record := s.Chains[name]
		subnetKey, chainIDKey, convertKey := StateChainKeys(name)
		fields = append(fields,
			struct{ key, value string }{subnetKey, idString(record.SubnetID)},
			struct{ key, value string }{chainIDKey, idString(record.ChainID)},
			struct{ key, value string }{convertKey, idString(record.ConvertTxID)},
		)
	}
	fields = append(fields,
		struct{ key, value string }{"MANAGER_ADDRESS", s.ManagerAddress},
		struct{ key, value string }{"GENESIS_EVM_ADDRESS", s.GenesisEVMAddress},
		struct{ key, value string }{"FEEDER_EVM_ADDRESS", s.FeederEVMAddress},
		struct{ key, value string }{"ORACLE_AGGREGATOR_ADDRESS", s.OracleAggregatorAddress},
		struct{ key, value string }{"ORACLE_RECEIVER_ADDRESS", s.OracleReceiverAddress},
		struct{ key, value string }{"ORACLE_PRICEFEED_ADDRESS", s.PriceFeedAddress},
		struct{ key, value string }{"ORACLE_PRICEFEED_AGGREGATOR_ADDRESS", s.PriceFeedAggregatorAddress},
	)
	var contents strings.Builder
	for _, field := range fields {
		if field.value != "" {
			fmt.Fprintf(&contents, "%s=%s\n", field.key, field.value)
		}
	}
	temp, err := os.CreateTemp(filepath.Dir(s.Path), ".network.env-*")
	if err != nil {
		return fmt.Errorf("create temporary state beside %s: %w", s.Path, err)
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return fmt.Errorf("protect temporary state %s: %w", tempPath, err)
	}
	if _, err := temp.WriteString(contents.String()); err != nil {
		temp.Close()
		return fmt.Errorf("write temporary state %s: %w", tempPath, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close temporary state %s: %w", tempPath, err)
	}
	if err := os.Rename(tempPath, s.Path); err != nil {
		return fmt.Errorf("publish state %s: %w", s.Path, err)
	}
	return nil
}

func idString(id ids.ID) string {
	if id == ids.Empty {
		return ""
	}
	return id.String()
}
