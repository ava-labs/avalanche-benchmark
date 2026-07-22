package weights

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/rpc"
	"github.com/ava-labs/avalanchego/vms/components/gas"
	"github.com/ava-labs/avalanchego/vms/platformvm"
)

type fakeClient struct {
	validators   map[ids.ID][]platformvm.ClientPermissionlessValidator
	l1Validators map[ids.ID]platformvm.L1Validator
	feePrice     gas.Price
	height       uint64
}

func (f fakeClient) GetCurrentValidators(_ context.Context, subnetID ids.ID, _ []ids.NodeID, _ ...rpc.Option) ([]platformvm.ClientPermissionlessValidator, error) {
	return f.validators[subnetID], nil
}

func (f fakeClient) GetValidatorFeeState(context.Context, ...rpc.Option) (gas.Gas, gas.Price, time.Time, error) {
	return 0, f.feePrice, time.Time{}, nil
}

func (f fakeClient) GetHeight(context.Context, ...rpc.Option) (uint64, error) {
	return f.height, nil
}

func (f fakeClient) GetL1Validator(_ context.Context, validationID ids.ID, _ ...rpc.Option) (platformvm.L1Validator, uint64, error) {
	return f.l1Validators[validationID], f.height, nil
}

func TestLoadDeploymentRequiresCompletedMatchingCreation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "network.env")
	managementChainID := ids.GenerateTestID()
	managementSubnetID := ids.GenerateTestID()
	mainChainID := ids.GenerateTestID()
	mainSubnetID := ids.GenerateTestID()
	convertTxID := ids.GenerateTestID()
	contents := strings.Join([]string{
		"NETWORK=fuji",
		"MANAGER_SUBNET_ID=" + managementSubnetID.String(),
		"MANAGER_CHAIN_ID=" + managementChainID.String(),
		"SUBNET_ID=" + mainSubnetID.String(),
		"CHAIN_ID=" + mainChainID.String(),
		"CONVERT_TX_ID=" + convertTxID.String(),
	}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	deployment, err := LoadDeployment(path, "fuji")
	if err != nil {
		t.Fatal(err)
	}
	if deployment.ManagementSubnetID != managementSubnetID || deployment.ManagementChainID != managementChainID || deployment.MainSubnetID != mainSubnetID || deployment.MainChainID != mainChainID {
		t.Fatalf("unexpected deployment: %+v", deployment)
	}
	if _, err := LoadDeployment(path, "mainnet"); err == nil {
		t.Fatal("network mismatch must fail")
	}
	if err := os.WriteFile(path, []byte("NETWORK=fuji\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDeployment(path, "fuji"); err == nil || !strings.Contains(err.Error(), "creation is incomplete") {
		t.Fatalf("expected incomplete creation error, got %v", err)
	}
}

func TestFetchSortsValidatorsAndCalculatesDaysAtCurrentPrice(t *testing.T) {
	firstNodeID := ids.GenerateTestNodeID()
	secondNodeID := ids.GenerateTestNodeID()
	if firstNodeID.String() > secondNodeID.String() {
		firstNodeID, secondNodeID = secondNodeID, firstNodeID
	}
	firstValidationID := ids.GenerateTestID()
	secondValidationID := ids.GenerateTestID()
	firstBalance := uint64(2 * secondsPerDay * 10)
	secondBalance := uint64(secondsPerDay * 10)
	managementSubnetID := ids.GenerateTestID()
	mainSubnetID := ids.GenerateTestID()
	managementValidationID := ids.GenerateTestID()
	managementBalance := uint64(3 * secondsPerDay * 10)
	managementNodeID := ids.GenerateTestNodeID()
	pChain := fakeClient{
		feePrice: 10,
		height:   100,
		validators: map[ids.ID][]platformvm.ClientPermissionlessValidator{
			managementSubnetID: {
				{
					ClientStaker:      platformvm.ClientStaker{NodeID: managementNodeID, Weight: 1000},
					ClientL1Validator: platformvm.ClientL1Validator{ValidationID: &managementValidationID, Balance: &managementBalance},
				},
			},
			mainSubnetID: {
				{
					ClientStaker:      platformvm.ClientStaker{NodeID: secondNodeID, Weight: 1000},
					ClientL1Validator: platformvm.ClientL1Validator{ValidationID: &secondValidationID, Balance: &secondBalance},
				},
				{
					ClientStaker:      platformvm.ClientStaker{NodeID: firstNodeID, Weight: 100000},
					ClientL1Validator: platformvm.ClientL1Validator{ValidationID: &firstValidationID, Balance: &firstBalance},
				},
			},
		},
		l1Validators: map[ids.ID]platformvm.L1Validator{
			managementValidationID: {NodeID: managementNodeID, Weight: 1000, Balance: managementBalance},
			firstValidationID:      {NodeID: firstNodeID, Weight: 100000, Balance: firstBalance},
			secondValidationID:     {NodeID: secondNodeID, Weight: 1000, Balance: secondBalance},
		},
	}
	managementChainID := ids.GenerateTestID()
	mainChainID := ids.GenerateTestID()
	report, err := fetch(context.Background(), pChain, Deployment{
		ManagementSubnetID: managementSubnetID,
		ManagementChainID:  managementChainID,
		MainSubnetID:       mainSubnetID,
		MainChainID:        mainChainID,
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.ManagementChainID != managementChainID || report.MainChainID != mainChainID || report.FeePrice != 10 {
		t.Fatalf("unexpected report metadata: %+v", report)
	}
	if len(report.Validators) != 3 || report.Validators[0].L1 != "management" || report.Validators[0].NodeID != managementNodeID || report.Validators[1].NodeID != firstNodeID {
		t.Fatalf("validator sets not grouped and sorted: %+v", report.Validators)
	}
	if report.Validators[0].DaysLeft != 3 || report.Validators[1].DaysLeft != 2 || report.Validators[2].DaysLeft != 1 {
		t.Fatalf("unexpected days left: %+v", report.Validators)
	}
}

func TestFetchActiveAllowsDestroyedValidatorSets(t *testing.T) {
	managementSubnetID := ids.GenerateTestID()
	mainSubnetID := ids.GenerateTestID()
	staleValidationID := ids.GenerateTestID()
	report, err := fetch(context.Background(), fakeClient{
		feePrice: 10,
		height:   100,
		validators: map[ids.ID][]platformvm.ClientPermissionlessValidator{
			mainSubnetID: {
				{
					ClientStaker:      platformvm.ClientStaker{NodeID: ids.GenerateTestNodeID(), Weight: 1000},
					ClientL1Validator: platformvm.ClientL1Validator{ValidationID: &staleValidationID},
				},
			},
		},
		l1Validators: map[ids.ID]platformvm.L1Validator{
			staleValidationID: {Balance: 0},
		},
	}, Deployment{
		ManagementSubnetID: managementSubnetID,
		ManagementChainID:  ids.GenerateTestID(),
		MainSubnetID:       mainSubnetID,
		MainChainID:        ids.GenerateTestID(),
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Validators) != 0 {
		t.Fatalf("expected no active validators, got %+v", report.Validators)
	}
	if _, err := fetch(context.Background(), fakeClient{
		feePrice:     10,
		height:       100,
		validators:   map[ids.ID][]platformvm.ClientPermissionlessValidator{},
		l1Validators: map[ids.ID]platformvm.L1Validator{},
	}, Deployment{
		ManagementSubnetID: managementSubnetID,
		MainSubnetID:       mainSubnetID,
	}, true); err == nil || !strings.Contains(err.Error(), "deployment has no active validators") {
		t.Fatalf("expected destroyed deployment error, got %v", err)
	}
}
