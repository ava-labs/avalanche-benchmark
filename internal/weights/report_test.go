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
	validators []platformvm.ClientPermissionlessValidator
	feePrice   gas.Price
}

func (f fakeClient) GetCurrentValidators(context.Context, ids.ID, []ids.NodeID, ...rpc.Option) ([]platformvm.ClientPermissionlessValidator, error) {
	return f.validators, nil
}

func (f fakeClient) GetValidatorFeeState(context.Context, ...rpc.Option) (gas.Gas, gas.Price, time.Time, error) {
	return 0, f.feePrice, time.Time{}, nil
}

func TestLoadDeploymentRequiresCompletedMatchingCreation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "network.env")
	managementChainID := ids.GenerateTestID()
	subnetID := ids.GenerateTestID()
	convertTxID := ids.GenerateTestID()
	contents := strings.Join([]string{
		"NETWORK=fuji",
		"MANAGER_CHAIN_ID=" + managementChainID.String(),
		"SUBNET_ID=" + subnetID.String(),
		"CONVERT_TX_ID=" + convertTxID.String(),
	}, "\n")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	deployment, err := LoadDeployment(path, "fuji")
	if err != nil {
		t.Fatal(err)
	}
	if deployment.ManagementChainID != managementChainID || deployment.SubnetID != subnetID {
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
	pChain := fakeClient{
		feePrice: 10,
		validators: []platformvm.ClientPermissionlessValidator{
			{
				ClientStaker:      platformvm.ClientStaker{NodeID: secondNodeID, Weight: 1000},
				ClientL1Validator: platformvm.ClientL1Validator{ValidationID: &secondValidationID, Balance: &secondBalance},
			},
			{
				ClientStaker:      platformvm.ClientStaker{NodeID: firstNodeID, Weight: 100000},
				ClientL1Validator: platformvm.ClientL1Validator{ValidationID: &firstValidationID, Balance: &firstBalance},
			},
		},
	}
	managementChainID := ids.GenerateTestID()
	report, err := fetch(context.Background(), pChain, Deployment{
		ManagementChainID: managementChainID,
		SubnetID:          ids.GenerateTestID(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.ManagementChainID != managementChainID || report.FeePrice != 10 {
		t.Fatalf("unexpected report metadata: %+v", report)
	}
	if len(report.Validators) != 2 || report.Validators[0].NodeID != firstNodeID {
		t.Fatalf("validators not sorted: %+v", report.Validators)
	}
	if report.Validators[0].DaysLeft != 2 || report.Validators[1].DaysLeft != 1 {
		t.Fatalf("unexpected days left: %+v", report.Validators)
	}
}
