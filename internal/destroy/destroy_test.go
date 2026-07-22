package destroy

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/weights"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	commonopts "github.com/ava-labs/avalanchego/wallet/subnet/primary/common"
)

type fakeWallet struct {
	validationIDs []ids.ID
}

func TestOwnedByRequiresExactSingleUnlockedOwner(t *testing.T) {
	address := ids.GenerateTestShortID()
	owner := &secp256k1fx.OutputOwners{
		Threshold: 1,
		Addrs:     []ids.ShortID{address},
	}
	if !ownedBy(owner, address) {
		t.Fatal("exact owner must pass")
	}
	owner.Locktime = 1
	if ownedBy(owner, address) {
		t.Fatal("locked owner must fail")
	}
	if ownedBy(nil, address) {
		t.Fatal("missing owner must fail")
	}
}

func TestRunWithoutConversionsNeedsNoPChain(t *testing.T) {
	var output bytes.Buffer
	if err := Run(t.Context(), config.Environment{}, weights.Deployment{}, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "nothing to reclaim") {
		t.Fatalf("unexpected output: %q", output.String())
	}
}

func (f *fakeWallet) IssueDisableL1ValidatorTx(validationID ids.ID, _ ...commonopts.Option) (*txs.Tx, error) {
	f.validationIDs = append(f.validationIDs, validationID)
	return &txs.Tx{TxID: ids.GenerateTestID()}, nil
}

func TestExecuteDisablesMainBeforeManagementAndReportsEveryTransaction(t *testing.T) {
	managementID := ids.GenerateTestID()
	mainID := ids.GenerateTestID()
	validators := reclaimableMainBeforeManagement([]weights.Validator{
		{L1: "management", NodeID: ids.GenerateTestNodeID(), ValidationID: managementID, Balance: 1},
		{L1: "main", NodeID: ids.GenerateTestNodeID(), ValidationID: mainID, Balance: 1},
	})
	wallet := &fakeWallet{}
	var output bytes.Buffer
	if err := execute(wallet, validators, &output); err != nil {
		t.Fatal(err)
	}
	if len(wallet.validationIDs) != 2 || wallet.validationIDs[0] != mainID || wallet.validationIDs[1] != managementID {
		t.Fatalf("unexpected disable order: %v", wallet.validationIDs)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "main") || !strings.Contains(lines[1], "management") {
		t.Fatalf("unexpected output: %q", output.String())
	}
}

func TestReclaimableSkipsStaleDisabledValidators(t *testing.T) {
	validators := reclaimableMainBeforeManagement([]weights.Validator{
		{L1: "main", ValidationID: ids.GenerateTestID(), Balance: 0},
		{L1: "management", ValidationID: ids.GenerateTestID(), Balance: 1},
	})
	if len(validators) != 1 || validators[0].L1 != "management" {
		t.Fatalf("unexpected reclaimable validators: %+v", validators)
	}
}
