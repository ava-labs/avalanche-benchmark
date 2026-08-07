package topup

import (
	"context"
	"fmt"
	"io"
	"math"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanche-benchmark/internal/funding"
	"github.com/ava-labs/avalanche-benchmark/internal/weights"
	"github.com/ava-labs/avalanchego/utils/units"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
)

const (
	secondsPerDay    = 24 * 60 * 60
	settlementBuffer = 60 * 60
	transactionFees  = units.Avax / 10
)

type action struct {
	validator weights.Validator
	shortfall uint64
}

func Run(
	ctx context.Context,
	environment config.Environment,
	deployment weights.Deployment,
	days uint64,
	output io.Writer,
) error {
	if days == 0 {
		return fmt.Errorf("topup days must be greater than zero")
	}
	report, err := weights.Fetch(ctx, environment.PChainAPI, deployment)
	if err != nil {
		return err
	}
	minimum, err := targetBalance(days, uint64(report.FeePrice))
	if err != nil {
		return err
	}
	buffer, err := balanceForSeconds(settlementBuffer, uint64(report.FeePrice))
	if err != nil {
		return err
	}
	topupTarget, err := add(minimum, buffer)
	if err != nil {
		return err
	}
	actions, totalShortfall, err := plan(report.Validators, minimum, topupTarget)
	if err != nil {
		return err
	}
	fmt.Fprintf(output, "minimum: %d days at %d nAVAX/second\n", days, report.FeePrice)
	if totalShortfall == 0 {
		return execute(nil, actions, uint64(report.FeePrice), output)
	}

	fundingInfo, err := funding.Inspect(ctx, environment)
	if err != nil {
		return fmt.Errorf("funding preflight: %w", err)
	}
	required, err := add(totalShortfall, transactionFees)
	if err != nil {
		return err
	}
	if fundingInfo.Balance < required {
		return fmt.Errorf(
			"funding preflight: P-chain address %s has %s, topup shortfalls plus fee reserve require %s; add AVAX and run `go run ./cmd/l1 address` before retrying",
			fundingInfo.Addresses.PChain,
			formatAVAX(fundingInfo.Balance),
			formatAVAX(required),
		)
	}

	fundingKey, err := funding.ParsePrivateKey(environment.FundingPrivateKey)
	if err != nil {
		return err
	}
	pKeychain := secp256k1fx.NewKeychain(fundingKey)
	pWallet, err := primary.MakePWallet(ctx, environment.PChainAPI, pKeychain, primary.WalletConfig{})
	if err != nil {
		return fmt.Errorf("connect P-chain wallet to %s: %w", environment.PChainAPI, err)
	}
	return execute(pWallet, actions, uint64(report.FeePrice), output)
}

func plan(validators []weights.Validator, minimum, topupTarget uint64) ([]action, uint64, error) {
	actions := make([]action, 0, len(validators))
	var total uint64
	for _, validator := range validators {
		shortfall := uint64(0)
		if validator.Balance < minimum {
			shortfall = topupTarget - validator.Balance
			var err error
			total, err = add(total, shortfall)
			if err != nil {
				return nil, 0, err
			}
		}
		actions = append(actions, action{validator: validator, shortfall: shortfall})
	}
	return actions, total, nil
}

func execute(pWallet wallet.Wallet, actions []action, feePrice uint64, output io.Writer) error {
	for _, action := range actions {
		validator := action.validator
		if action.shortfall == 0 {
			fmt.Fprintf(output, "%s %s: already had %.2f days\n", validator.L1, validator.NodeID, daysLeft(validator.Balance, feePrice))
			continue
		}
		tx, err := pWallet.IssueIncreaseL1ValidatorBalanceTx(validator.ValidationID, action.shortfall)
		if err != nil {
			return fmt.Errorf("top up %s validator %s: %w", validator.L1, validator.NodeID, err)
		}
		fmt.Fprintf(output, "%s %s: tx %s\n", validator.L1, validator.NodeID, tx.ID())
	}
	return nil
}

func targetBalance(days, feePrice uint64) (uint64, error) {
	if feePrice == 0 {
		return 0, fmt.Errorf("current validator fee price is zero")
	}
	if days > math.MaxUint64/secondsPerDay {
		return 0, fmt.Errorf("topup days %d overflows seconds", days)
	}
	return balanceForSeconds(days*secondsPerDay, feePrice)
}

func balanceForSeconds(seconds, feePrice uint64) (uint64, error) {
	if feePrice == 0 {
		return 0, fmt.Errorf("current validator fee price is zero")
	}
	if seconds > math.MaxUint64/feePrice {
		return 0, fmt.Errorf("topup target overflows balance")
	}
	return seconds * feePrice, nil
}

func daysLeft(balance, feePrice uint64) float64 {
	return float64(balance) / float64(feePrice) / secondsPerDay
}

func add(a, b uint64) (uint64, error) {
	if a > math.MaxUint64-b {
		return 0, fmt.Errorf("topup balance overflows uint64")
	}
	return a + b, nil
}

func formatAVAX(amount uint64) string {
	return fmt.Sprintf("%d.%09d AVAX", amount/units.Avax, amount%units.Avax)
}
