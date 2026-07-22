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
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	ethcommon "github.com/ava-labs/libevm/common"
	"github.com/joho/godotenv"
)

const secondsPerDay = 24 * 60 * 60

const (
	stateReadAttempts = 10
	stateReadDelay    = time.Second
)

type Deployment struct {
	ManagementSubnetID    ids.ID
	ManagementChainID     ids.ID
	ManagementConvertTxID ids.ID
	MainSubnetID          ids.ID
	MainChainID           ids.ID
	ManagerAddress        ethcommon.Address
}

type Validator struct {
	L1                    string
	NodeID                ids.NodeID
	ValidationID          ids.ID
	Weight                uint64
	Balance               uint64
	DaysLeft              float64
	RemainingBalanceOwner *secp256k1fx.OutputOwners
	DeactivationOwner     *secp256k1fx.OutputOwners
}

type Report struct {
	ManagementChainID ids.ID
	MainChainID       ids.ID
	FeePrice          gas.Price
	Validators        []Validator
}

type client interface {
	GetCurrentValidators(context.Context, ids.ID, []ids.NodeID, ...rpc.Option) ([]platformvm.ClientPermissionlessValidator, error)
	GetHeight(context.Context, ...rpc.Option) (uint64, error)
	GetL1Validator(context.Context, ids.ID, ...rpc.Option) (platformvm.L1Validator, uint64, error)
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
	managementConvertTxID, err := requiredID(path, values, "MANAGER_CONVERT_TX_ID")
	if err != nil {
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
	managerAddressRaw := strings.TrimSpace(values["MANAGER_ADDRESS"])
	if !ethcommon.IsHexAddress(managerAddressRaw) {
		return Deployment{}, fmt.Errorf("%s: required field MANAGER_ADDRESS must be a hex address, got %q", path, managerAddressRaw)
	}
	return Deployment{
		ManagementSubnetID:    managementSubnetID,
		ManagementChainID:     managementChainID,
		ManagementConvertTxID: managementConvertTxID,
		MainSubnetID:          mainSubnetID,
		MainChainID:           mainChainID,
		ManagerAddress:        ethcommon.HexToAddress(managerAddressRaw),
	}, nil
}

func Fetch(ctx context.Context, api string, deployment Deployment) (Report, error) {
	return fetch(ctx, platformvm.NewClient(api), deployment, true)
}

// FetchActive is equivalent to Fetch but permits an empty validator set. It is
// used by destroy so a partially completed destruction can be resumed.
func FetchActive(ctx context.Context, api string, deployment Deployment) (Report, error) {
	return fetch(ctx, platformvm.NewClient(api), deployment, false)
}

func fetch(ctx context.Context, pChain client, deployment Deployment, requireValidators bool) (Report, error) {
	height, err := pChain.GetHeight(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("read P-chain height: %w", err)
	}
	_, feePrice, _, err := pChain.GetValidatorFeeState(ctx)
	if err != nil {
		return Report{}, fmt.Errorf("read current validator fee price: %w", err)
	}
	if feePrice == 0 {
		return Report{}, fmt.Errorf("current validator fee price is zero")
	}

	management, err := fetchValidators(ctx, pChain, "management", deployment.ManagementSubnetID, height, feePrice)
	if err != nil {
		return Report{}, err
	}
	main, err := fetchValidators(ctx, pChain, "main", deployment.MainSubnetID, height, feePrice)
	if err != nil {
		return Report{}, err
	}
	rows := append(management, main...)
	if requireValidators && len(rows) == 0 {
		return Report{}, fmt.Errorf("deployment has no active validators; it is destroyed")
	}
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

func fetchValidators(ctx context.Context, pChain client, l1 string, subnetID ids.ID, minimumHeight uint64, feePrice gas.Price) ([]Validator, error) {
	validators, err := pChain.GetCurrentValidators(ctx, subnetID, nil)
	if err != nil {
		return nil, fmt.Errorf("read %s validators for %s: %w", l1, subnetID, err)
	}
	rows := make([]Validator, 0, len(validators))
	for _, validator := range validators {
		if validator.ValidationID == nil {
			return nil, fmt.Errorf("%s validator %s is missing L1 validation state", l1, validator.NodeID)
		}
		l1Validator, err := getL1ValidatorAt(ctx, pChain, *validator.ValidationID, minimumHeight)
		if err != nil {
			return nil, fmt.Errorf("read %s validator %s: %w", l1, validator.NodeID, err)
		}
		balance := l1Validator.Balance
		// Membership responses can lag committed disables. A zero balance from a
		// height-consistent L1 record is terminal, so treating it as inactive
		// prevents reports and lifecycle commands from acting on destroyed state.
		if balance == 0 {
			continue
		}
		secondsLeft := balance / uint64(feePrice)
		rows = append(rows, Validator{
			L1:                    l1,
			NodeID:                l1Validator.NodeID,
			ValidationID:          *validator.ValidationID,
			Weight:                l1Validator.Weight,
			Balance:               balance,
			DaysLeft:              float64(secondsLeft) / secondsPerDay,
			RemainingBalanceOwner: l1Validator.RemainingBalanceOwner,
			DeactivationOwner:     l1Validator.DeactivationOwner,
		})
	}
	return rows, nil
}

func getL1ValidatorAt(ctx context.Context, pChain client, validationID ids.ID, minimumHeight uint64) (platformvm.L1Validator, error) {
	var lastHeight uint64
	for attempt := 0; attempt < stateReadAttempts; attempt++ {
		validator, height, err := pChain.GetL1Validator(ctx, validationID)
		if err == nil && height >= minimumHeight {
			return validator, nil
		}
		if err != nil {
			if attempt == stateReadAttempts-1 {
				return platformvm.L1Validator{}, err
			}
		} else {
			lastHeight = height
		}
		select {
		case <-ctx.Done():
			return platformvm.L1Validator{}, ctx.Err()
		case <-time.After(stateReadDelay):
		}
	}
	return platformvm.L1Validator{}, fmt.Errorf("stale P-chain response at height %d, need at least %d", lastHeight, minimumHeight)
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
