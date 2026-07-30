package creation

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ava-labs/avalanchego/ids"
)

type State struct {
	Path                    string
	Network                 string
	ManagerSubnetID         ids.ID
	ManagerChainID          ids.ID
	ManagerConvertTxID      ids.ID
	OracleSubnetID          ids.ID
	OracleChainID           ids.ID
	OracleConvertTxID       ids.ID
	SubnetID                ids.ID
	ChainID                 ids.ID
	ConvertTxID             ids.ID
	ManagerAddress          string
	GenesisEVMAddress       string
	FeederEVMAddress        string
	OracleAggregatorAddress string
	OracleReceiverAddress   string
	PriceFeedAddress        string
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
		{"MANAGER_ADDRESS", s.ManagerAddress},
		{"GENESIS_EVM_ADDRESS", s.GenesisEVMAddress},
		{"FEEDER_EVM_ADDRESS", s.FeederEVMAddress},
		{"ORACLE_AGGREGATOR_ADDRESS", s.OracleAggregatorAddress},
		{"ORACLE_RECEIVER_ADDRESS", s.OracleReceiverAddress},
		{"ORACLE_PRICEFEED_ADDRESS", s.PriceFeedAddress},
	}
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
