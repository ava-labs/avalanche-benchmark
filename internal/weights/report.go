package weights

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/rpc"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/joho/godotenv"
)

const secondsPerDay = 24 * 60 * 60

type Deployment struct {
	ManagementSubnetID ids.ID
	ManagementChainID  ids.ID
	MainSubnetID       ids.ID
	MainChainID        ids.ID
}

type Validator struct {
	L1       string
	NodeID   ids.NodeID
	Weight   uint64
	Balance  uint64
	DaysLeft float64
}

type Report struct {
	ManagementChainID ids.ID
	MainChainID       ids.ID
	FeePrice          gas.Price
	Validators        []Validator
}

type client interface {
	GetCurrentValidators(context.Context, ids.ID, []ids.NodeID, ...rpc.Option) ([]platformvm.ClientPermissionlessValidator, error)
	GetValidatorFeeState(context.Context, ...rpc.Option) (gas.Gas, gas.Price, time.Time, error)
}

func LoadDeployment(path, network string) (Deployment, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		return Deployment{}, fmt.Errorf("read required creation state %s: %w", path, err)
	}
	if got := strings.TrimSpace(values["NETWORK"]); got != network {
		return Deployment{}, fmt.Errorf("%s: NETWORK must match .env (%q), got %q", path, network, got)
	}
	if strings.TrimSpace(values["CONVERT_TX_ID"]) == "" {
		return Deployment{}, fmt.Errorf("%s: required field CONVERT_TX_ID is not provided; creation is incomplete", path)
	}
	if _, err := requiredID(path, values, "CONVERT_TX_ID"); err != nil {
		return Deployment{}, err
	}
	managementChainID, err := requiredID(path, values, "MANAGER_CHAIN_ID")
	if err != nil {
		return Deployment{}, err
	}
	managementSubnetID, err := requiredID(path, values, "MANAGER_SUBNET_ID")
	if err != nil {
		return Deployment{}, err
	}
	mainChainID, err := requiredID(path, values, "CHAIN_ID")
	if err != nil {
		return Deployment{}, err
	}
	mainSubnetID, err := requiredID(path, values, "SUBNET_ID")
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{
		ManagementSubnetID: managementSubnetID,
		ManagementChainID:  managementChainID,
		MainSubnetID:       mainSubnetID,
		MainChainID:        mainChainID,
	}, nil
}

func Fetch(ctx context.Context, api string, deployment Deployment) (Report, error) {
	return fetch(ctx, platformvm.NewClient(api), deployment)
}

func fetch(ctx context.Context, pChain client, deployment Deployment) (Report, error) {
	_, feePrice, _, err := pChain.GetValidatorFeeState(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("read current validator fee price: %w", err)
	}
	if feePrice == 0 {
		return Report{}, fmt.Errorf("current validator fee price is zero")
	}

	management, err := fetchValidators(ctx, pChain, "management", deployment.ManagementSubnetID, feePrice)
	if err != nil {
		return Report{}, err
	}
	main, err := fetchValidators(ctx, pChain, "main", deployment.MainSubnetID, feePrice)
	if err != nil {
		return Report{}, err
	}
	rows := append(management, main...)
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].L1 != rows[j].L1 {
			return rows[i].L1 == "management"
		}
		return rows[i].NodeID.String() < rows[j].NodeID.String()
	})
	return Report{
		ManagementChainID: deployment.ManagementChainID,
		MainChainID:       deployment.MainChainID,
		FeePrice:          feePrice,
		Validators:        rows,
	}, nil
}

func fetchValidators(ctx context.Context, pChain client, l1 string, subnetID ids.ID, feePrice gas.Price) ([]Validator, error) {
	validators, err := pChain.GetCurrentValidators(ctx, subnetID, nil)
	if err != nil {
		return nil, fmt.Errorf("read %s validators for %s: %w", l1, subnetID, err)
	}
	if len(validators) == 0 {
		return nil, fmt.Errorf("read %s validators for %s: no validators returned", l1, subnetID)
	}
	rows := make([]Validator, 0, len(validators))
	for _, validator := range validators {
		if validator.ValidationID == nil || validator.Balance == nil {
			return nil, fmt.Errorf("%s validator %s is missing L1 validation state", l1, validator.NodeID)
		}
		balance := *validator.Balance
		secondsLeft := balance / uint64(feePrice)
		rows = append(rows, Validator{
			L1:       l1,
			NodeID:   validator.NodeID,
			Weight:   validator.Weight,
			Balance:  balance,
			DaysLeft: float64(secondsLeft) / secondsPerDay,
		})
	}
	return rows, nil
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
