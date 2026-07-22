package setweight

import (
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/funding"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/weights"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/staking"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/rpc"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
	warppayload "github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	commonopts "github.com/ava-labs/avalanchego/wallet/subnet/primary/common"
)

const (
	DeadWeight   uint64 = 1
	SpareWeight  uint64 = 1000
	ActiveWeight uint64 = 100000

	quorumNumerator   = 67
	quorumDenominator = 100
	readAttempts      = 10
	readDelay         = time.Second
	verifyTimeout     = 90 * time.Second
)

type client interface {
	GetCurrentValidators(context.Context, ids.ID, []ids.NodeID, ...rpc.Option) ([]platformvm.ClientPermissionlessValidator, error)
	GetHeight(context.Context, ...rpc.Option) (uint64, error)
	GetL1Validator(context.Context, ids.ID, ...rpc.Option) (platformvm.L1Validator, uint64, error)
}

type wallet interface {
	IssueSetL1ValidatorWeightTx([]byte, ...commonopts.Option) (*txs.Tx, error)
}

type registeredValidator struct {
	NodeID       ids.NodeID
	ValidationID ids.ID
	Weight       uint64
	Balance      uint64
	MinNonce     uint64
	PublicKey    *bls.PublicKey
}

func Run(
	ctx context.Context,
	cfg config.Config,
	deployment weights.Deployment,
	deploymentDirectory string,
	identityNumber int,
	targetWeight uint64,
	output io.Writer,
) error {
	if err := ValidateWeight(targetWeight); err != nil {
		return err
	}
	if err := validateIdentity(cfg.Nodes, identityNumber); err != nil {
		return err
	}
	targetNodeID, err := nodeIDFromCertificate(filepath.Join(
		deploymentDirectory,
		"nodes",
		strconv.Itoa(identityNumber),
		"staker.crt",
	))
	if err != nil {
		return fmt.Errorf("load validator identity %d: %w", identityNumber, err)
	}

	pChain := platformvm.NewClient(cfg.Environment.PChainAPI)
	height, err := pChain.GetHeight(ctx)
	if err != nil {
		return fmt.Errorf("read P-chain height: %w", err)
	}
	mainValidators, err := fetchValidatorsAt(ctx, pChain, deployment.MainSubnetID, height)
	if err != nil {
		return fmt.Errorf("read main validators: %w", err)
	}
	target, err := findTarget(mainValidators, targetNodeID)
	if err != nil {
		return fmt.Errorf("identity %d: %w", identityNumber, err)
	}
	if target.Balance == 0 {
		return fmt.Errorf("identity %d validator %s is inactive", identityNumber, target.NodeID)
	}
	if target.Weight == targetWeight {
		fmt.Fprintf(output, "identity %d %s: already weight %d\n", identityNumber, target.NodeID, targetWeight)
		return nil
	}

	managementValidators, err := fetchValidatorsAt(ctx, pChain, deployment.ManagementSubnetID, height)
	if err != nil {
		return fmt.Errorf("read management validators: %w", err)
	}
	if len(managementValidators) != cfg.Environment.ManagerCommittee {
		return fmt.Errorf("management validator count is %d, expected %d", len(managementValidators), cfg.Environment.ManagerCommittee)
	}
	warpSet, err := canonicalWarpSet(managementValidators)
	if err != nil {
		return fmt.Errorf("build management signing set: %w", err)
	}
	signers, err := loadManagerSigners(filepath.Join(deploymentDirectory, "manager"), cfg.Environment.ManagerCommittee)
	if err != nil {
		return err
	}
	networkID, err := constants.NetworkID(cfg.Environment.Network)
	if err != nil {
		return fmt.Errorf("resolve network %q: %w", cfg.Environment.Network, err)
	}
	payload, err := warpmessage.NewL1ValidatorWeight(target.ValidationID, target.MinNonce, targetWeight)
	if err != nil {
		return fmt.Errorf("build L1ValidatorWeight: %w", err)
	}
	call, err := warppayload.NewAddressedCall(deployment.ManagerAddress.Bytes(), payload.Bytes())
	if err != nil {
		return fmt.Errorf("build addressed call: %w", err)
	}
	unsigned, err := warp.NewUnsignedMessage(networkID, deployment.ManagementChainID, call.Bytes())
	if err != nil {
		return fmt.Errorf("build unsigned Warp message: %w", err)
	}
	signed, err := signAndAggregate(unsigned, warpSet, signers)
	if err != nil {
		return err
	}

	fundingKey, err := funding.ParsePrivateKey(cfg.Environment.FundingPrivateKey)
	if err != nil {
		return err
	}
	pWallet, err := primary.MakePWallet(
		ctx,
		cfg.Environment.PChainAPI,
		secp256k1fx.NewKeychain(fundingKey),
		primary.WalletConfig{},
	)
	if err != nil {
		return fmt.Errorf("connect P-chain wallet to %s: %w", cfg.Environment.PChainAPI, err)
	}
	return submitAndVerify(ctx, pChain, pWallet, signed, identityNumber, target, targetWeight, output)
}

func ValidateWeight(weight uint64) error {
	switch weight {
	case DeadWeight, SpareWeight, ActiveWeight:
		return nil
	default:
		return fmt.Errorf("set-weight weight must be 1, 1000, or 100000, got %d", weight)
	}
}

func validateIdentity(nodes []config.Node, number int) error {
	for _, node := range nodes {
		if node.Number != number {
			continue
		}
		if node.Role != config.RoleValidator {
			return fmt.Errorf("identity %d is an rpc identity; set-weight accepts validators only", number)
		}
		return nil
	}
	return fmt.Errorf("validator identity %d is not declared in nodes.ini", number)
}

func nodeIDFromCertificate(path string) (ids.NodeID, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return ids.EmptyNodeID, fmt.Errorf("read %s: %w", path, err)
	}
	block, _ := pem.Decode(contents)
	if block == nil {
		return ids.EmptyNodeID, fmt.Errorf("%s has no PEM block", path)
	}
	certificate, err := staking.ParseCertificate(block.Bytes)
	if err != nil {
		return ids.EmptyNodeID, fmt.Errorf("parse %s: %w", path, err)
	}
	return ids.NodeIDFromCert(certificate), nil
}

func fetchValidatorsAt(ctx context.Context, pChain client, subnetID ids.ID, minimumHeight uint64) ([]registeredValidator, error) {
	members, err := pChain.GetCurrentValidators(ctx, subnetID, nil)
	if err != nil {
		return nil, fmt.Errorf("getCurrentValidators %s: %w", subnetID, err)
	}
	validators := make([]registeredValidator, 0, len(members))
	for _, member := range members {
		if member.ValidationID == nil {
			return nil, fmt.Errorf("validator %s has no validation ID", member.NodeID)
		}
		state, err := getL1ValidatorAt(ctx, pChain, *member.ValidationID, minimumHeight)
		if err != nil {
			return nil, fmt.Errorf("getL1Validator %s: %w", *member.ValidationID, err)
		}
		validators = append(validators, registeredValidator{
			NodeID:       state.NodeID,
			ValidationID: *member.ValidationID,
			Weight:       state.Weight,
			Balance:      state.Balance,
			MinNonce:     state.MinNonce,
			PublicKey:    state.PublicKey,
		})
	}
	return validators, nil
}

func getL1ValidatorAt(ctx context.Context, pChain client, validationID ids.ID, minimumHeight uint64) (platformvm.L1Validator, error) {
	var lastHeight uint64
	for attempt := 0; attempt < readAttempts; attempt++ {
		validator, height, err := pChain.GetL1Validator(ctx, validationID)
		if err == nil && height >= minimumHeight {
			return validator, nil
		}
		if err != nil && attempt == readAttempts-1 {
			return platformvm.L1Validator{}, err
		}
		lastHeight = height
		select {
		case <-ctx.Done():
			return platformvm.L1Validator{}, ctx.Err()
		case <-time.After(readDelay):
		}
	}
	return platformvm.L1Validator{}, fmt.Errorf("stale P-chain response at height %d, need at least %d", lastHeight, minimumHeight)
}

func findTarget(validators []registeredValidator, nodeID ids.NodeID) (registeredValidator, error) {
	for _, validator := range validators {
		if validator.NodeID == nodeID {
			return validator, nil
		}
	}
	return registeredValidator{}, fmt.Errorf("validator %s is not active on the main L1", nodeID)
}

func canonicalWarpSet(registered []registeredValidator) (validators.WarpSet, error) {
	validatorSet := make(map[ids.NodeID]*validators.GetValidatorOutput, len(registered))
	for _, validator := range registered {
		publicKey := validator.PublicKey
		if validator.Weight == 0 || validator.Balance == 0 {
			publicKey = nil
		}
		validatorSet[validator.NodeID] = &validators.GetValidatorOutput{
			NodeID:    validator.NodeID,
			PublicKey: publicKey,
			Weight:    validator.Weight,
		}
	}
	return validators.FlattenValidatorSet(validatorSet)
}

func loadManagerSigners(directory string, count int) ([]bls.Signer, error) {
	signers := make([]bls.Signer, 0, count)
	for i := 1; i <= count; i++ {
		path := filepath.Join(directory, strconv.Itoa(i), "signer.key")
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read management signer %s: %w", path, err)
		}
		signer, err := localsigner.FromBytes(contents)
		if err != nil {
			return nil, fmt.Errorf("load management signer %s: %w", path, err)
		}
		signers = append(signers, signer)
	}
	return signers, nil
}

func signAndAggregate(unsigned *warp.UnsignedMessage, validatorSet validators.WarpSet, signers []bls.Signer) (*warp.Message, error) {
	byPublicKey := make(map[string]bls.Signer, len(signers))
	for _, signer := range signers {
		byPublicKey[string(bls.PublicKeyToUncompressedBytes(signer.PublicKey()))] = signer
	}

	signerBits := set.NewBits()
	signatures := make([]*bls.Signature, 0, len(signers))
	for index, validator := range validatorSet.Validators {
		signer, ok := byPublicKey[string(validator.PublicKeyBytes)]
		if !ok {
			continue
		}
		signature, err := signer.Sign(unsigned.Bytes())
		if err != nil {
			return nil, fmt.Errorf("sign Warp message: %w", err)
		}
		signatures = append(signatures, signature)
		signerBits.Add(index)
	}
	if len(signatures) == 0 {
		return nil, fmt.Errorf("none of the %d local management keys are in the %d-validator canonical signing set", len(signers), len(validatorSet.Validators))
	}
	aggregated, err := bls.AggregateSignatures(signatures)
	if err != nil {
		return nil, fmt.Errorf("aggregate Warp signatures: %w", err)
	}
	bitSetSignature := &warp.BitSetSignature{Signers: signerBits.Bytes()}
	copy(bitSetSignature.Signature[:], bls.SignatureToBytes(aggregated))
	if err := bitSetSignature.Verify(unsigned, unsigned.NetworkID, validatorSet, quorumNumerator, quorumDenominator); err != nil {
		return nil, fmt.Errorf("local management quorum verification: %w", err)
	}
	message, err := warp.NewMessage(unsigned, bitSetSignature)
	if err != nil {
		return nil, fmt.Errorf("build signed Warp message: %w", err)
	}
	return message, nil
}

func submitAndVerify(
	ctx context.Context,
	pChain client,
	pWallet wallet,
	message *warp.Message,
	identityNumber int,
	validator registeredValidator,
	targetWeight uint64,
	output io.Writer,
) error {
	tx, err := pWallet.IssueSetL1ValidatorWeightTx(
		message.Bytes(),
		commonopts.WithPollFrequency(time.Second),
	)
	if err != nil {
		return fmt.Errorf("set identity %d validator %s weight %d -> %d: %w", identityNumber, validator.NodeID, validator.Weight, targetWeight, err)
	}
	fmt.Fprintf(output, "identity %d %s: weight %d -> %d, tx %s\n", identityNumber, validator.NodeID, validator.Weight, targetWeight, tx.ID())

	deadline := time.Now().Add(verifyTimeout)
	for {
		state, _, readErr := pChain.GetL1Validator(ctx, validator.ValidationID)
		if readErr == nil && state.Weight == targetWeight {
			fmt.Fprintf(output, "identity %d %s: verified weight %d\n", identityNumber, validator.NodeID, targetWeight)
			return nil
		}
		if time.Now().After(deadline) {
			if readErr != nil {
				return fmt.Errorf("verify identity %d weight: %w", identityNumber, readErr)
			}
			return fmt.Errorf("verify identity %d weight: still %d, expected %d after %s", identityNumber, state.Weight, targetWeight, verifyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
