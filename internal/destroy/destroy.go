package destroy

import (
	"context"
	"fmt"
	"io"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/funding"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/weights"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	commonopts "github.com/ava-labs/avalanchego/wallet/subnet/primary/common"
)

type disableWallet interface {
	IssueDisableL1ValidatorTx(ids.ID, ...commonopts.Option) (*txs.Tx, error)
}

func Run(
	ctx context.Context,
	environment config.Environment,
	deployment weights.Deployment,
	output io.Writer,
) error {
	if deployment.MainSubnetID == ids.Empty && deployment.ManagementSubnetID == ids.Empty {
		fmt.Fprintln(output, "no converted validators; nothing to reclaim")
		return nil
	}
	fundingKey, err := funding.ParsePrivateKey(environment.FundingPrivateKey)
	if err != nil {
		return err
	}
	addresses, err := funding.DeriveAddresses(environment.Network, fundingKey)
	if err != nil {
		return err
	}
	report, err := weights.FetchActive(ctx, environment.PChainAPI, deployment)
	if err != nil {
		return err
	}
	validators := reclaimableMainBeforeManagement(report.Validators)
	if len(validators) == 0 {
		fmt.Fprintln(output, "no active validators; balances already reclaimed")
		return nil
	}
	for _, validator := range validators {
		if !ownedBy(validator.DeactivationOwner, fundingKey.Address()) {
			return fmt.Errorf("preflight %s validator %s: FUNDING_PRIVATE_KEY is not the deactivation owner", validator.L1, validator.NodeID)
		}
		if !ownedBy(validator.RemainingBalanceOwner, fundingKey.Address()) {
			return fmt.Errorf("preflight %s validator %s: FUNDING_PRIVATE_KEY is not the remaining balance owner", validator.L1, validator.NodeID)
		}
	}
	validationIDs := make([]ids.ID, len(validators))
	for i, validator := range validators {
		validationIDs[i] = validator.ValidationID
	}

	pWallet, err := primary.MakePWallet(
		ctx,
		environment.PChainAPI,
		secp256k1fx.NewKeychain(fundingKey),
		primary.WalletConfig{ValidationIDs: validationIDs},
	)
	if err != nil {
		return fmt.Errorf("connect P-chain wallet to %s: %w", environment.PChainAPI, err)
	}
	fmt.Fprintf(output, "destroying %d validators; all remaining balances return to %s\n", len(validators), addresses.PChain)
	return execute(pWallet, validators, output)
}

func reclaimableMainBeforeManagement(validators []weights.Validator) []weights.Validator {
	ordered := make([]weights.Validator, 0, len(validators))
	for _, l1 := range []string{"main", "oracle", "management"} {
		for _, validator := range validators {
			if validator.L1 == l1 && validator.Balance > 0 {
				ordered = append(ordered, validator)
			}
		}
	}
	return ordered
}

func ownedBy(owner *secp256k1fx.OutputOwners, address ids.ShortID) bool {
	return owner != nil &&
		owner.Locktime == 0 &&
		owner.Threshold == 1 &&
		len(owner.Addrs) == 1 &&
		owner.Addrs[0] == address
}

func execute(pWallet disableWallet, validators []weights.Validator, output io.Writer) error {
	for _, validator := range validators {
		tx, err := pWallet.IssueDisableL1ValidatorTx(validator.ValidationID)
		if err != nil {
			return fmt.Errorf("disable %s validator %s: %w", validator.L1, validator.NodeID, err)
		}
		fmt.Fprintf(output, "%s %s: tx %s, remaining balance returned\n", validator.L1, validator.NodeID, tx.ID())
	}
	return nil
}
