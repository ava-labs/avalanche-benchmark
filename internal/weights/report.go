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
	ManagementChainID ids.ID
	SubnetID          ids.ID
}

type Validator struct {
	NodeID   ids.NodeID
	Weight   uint64
	Balance  uint64
	DaysLeft float64
}

type Report struct {
	ManagementChainID ids.ID
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
	subnetID, err := requiredID(path, values, "SUBNET_ID")
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{
		ManagementChainID: managementChainID,
		SubnetID:          subnetID,
	}, nil
}

func Fetch(ctx context.Context, api string, deployment Deployment) (Report, error) {
	return fetch(ctx, platformvm.NewClient(api), deployment)
}

func fetch(ctx context.Context, pChain client, deployment Deployment) (Report, error) {
	validators, err := pChain.GetCurrentValidators(ctx, deployment.SubnetID, nil)
	if err != nil {
		return Report{}, fmt.Errorf("read current validators for %s: %w", deployment.SubnetID, err)
	}
	if len(validators) == 0 {
		return Report{}, fmt.Errorf("read current validators for %s: no validators returned", deployment.SubnetID)
	}
	_, feePrice, _, err := pChain.GetValidatorFeeState(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("read current validator fee price: %w", err)
	}
	if feePrice == 0 {
		return Report{}, fmt.Errorf("current validator fee price is zero")
	}

	rows := make([]Validator, 0, len(validators))
	for _, validator := range validators {
		if validator.ValidationID == nil || validator.Balance == nil {
			return Report{}, fmt.Errorf("validator %s is missing L1 validation state", validator.NodeID)
		}
		balance := *validator.Balance
		secondsLeft := balance / uint64(feePrice)
		rows = append(rows, Validator{
			NodeID:   validator.NodeID,
			Weight:   validator.Weight,
			Balance:  balance,
			DaysLeft: float64(secondsLeft) / secondsPerDay,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].NodeID.String() < rows[j].NodeID.String()
	})
	return Report{
		ManagementChainID: deployment.ManagementChainID,
		FeePrice:          feePrice,
		Validators:        rows,
	}, nil
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
