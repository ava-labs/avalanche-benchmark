package creation

import (
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanchego/ids"
	plformsigner "github.com/ava-labs/avalanchego/vms/platformvm/signer"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
)

func TestConversionValidatorsRejectRPC(t *testing.T) {
	identities := []identity.Identity{{Name: "node-1", Role: config.RoleRPC, NodeID: ids.GenerateTestNodeID()}}
	if _, err := conversionValidators(identities, func(identity.Identity) uint64 { return 1 }, warpmessage.PChainOwner{}); err == nil {
		t.Fatal("expected missing proof error")
	}
}

func TestConversionValidatorsSortByNodeIDAndKeepWeight(t *testing.T) {
	lowID := ids.GenerateTestNodeID()
	highID := ids.GenerateTestNodeID()
	if lowID.Compare(highID) > 0 {
		lowID, highID = highID, lowID
	}
	proof := &plformsigner.ProofOfPossession{}
	identities := []identity.Identity{
		{Name: "high", NodeID: highID, Proof: proof},
		{Name: "low", NodeID: lowID, Proof: proof},
	}
	validators, err := conversionValidators(identities, func(generated identity.Identity) uint64 {
		if generated.Name == "high" {
			return HighWeight
		}
		return LowWeight
	}, warpmessage.PChainOwner{})
	if err != nil {
		t.Fatal(err)
	}
	if string(validators[0].NodeID) != string(lowID.Bytes()) || validators[0].Weight != LowWeight {
		t.Fatalf("unexpected first validator: %+v", validators[0])
	}
	if string(validators[1].NodeID) != string(highID.Bytes()) || validators[1].Weight != HighWeight {
		t.Fatalf("unexpected second validator: %+v", validators[1])
	}
}
