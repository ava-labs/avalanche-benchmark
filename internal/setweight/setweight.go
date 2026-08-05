package setweight

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/creation"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/funding"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/weights"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/upgrade"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/ava-labs/avalanchego/utils/rpc"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/vms/platformvm"
	pblock "github.com/ava-labs/avalanchego/vms/platformvm/block"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	warpmessage "github.com/ava-labs/avalanchego/vms/platformvm/warp/message"
	warppayload "github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"
	"github.com/ava-labs/avalanchego/vms/proposervm"
	proposerblock "github.com/ava-labs/avalanchego/vms/proposervm/block"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
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

type epochClient interface {
	GetCurrentEpoch(context.Context, ...rpc.Option) (proposerblock.Epoch, error)
}

type blockReader interface {
	Block(context.Context, uint64) (time.Time, []ids.ID, error)
}

type platformBlockClient interface {
	GetBlockByHeight(context.Context, uint64, ...rpc.Option) ([]byte, error)
}

type platformBlockReader struct {
	client platformBlockClient
}

type registeredValidator struct {
	NodeID       ids.NodeID
	ValidationID ids.ID
	Weight       uint64
	Balance      uint64
	MinNonce     uint64
	PublicKey    *bls.PublicKey
	StartTime    uint64
}

func Run(
	ctx context.Context,
	environment config.Environment,
	deployment weights.Deployment,
	deploymentDirectory string,
	identityName string,
	targetWeight uint64,
	output io.Writer,
) error {
	if err := ValidateWeight(targetWeight); err != nil {
		return err
	}
	public, _, err := creation.LoadPublic(filepath.Join(deploymentDirectory, "public.json"))
	if err != nil {
		return err
	}
	targetPublic, err := validateIdentity(public.Nodes, identityName)
	if err != nil {
		return err
	}
	targetNodeID, err := ids.NodeIDFromString(targetPublic.NodeID)
	if err != nil {
		return fmt.Errorf("identity %s nodeID: %w", identityName, err)
	}
	// The identity names its chain, so the command reads the right subnet
	// without any flag: one committee manages every chain, and the Warp
	// message below is chain-agnostic.
	chainName := targetPublic.ChainName()
	targetSubnetID := deployment.MainSubnetID
	if chainName != config.MainChain {
		record, known := deployment.Chains[chainName]
		if !known {
			return fmt.Errorf("identity %s validates chain %q, but network.env records no such chain", identityName, chainName)
		}
		targetSubnetID = record.SubnetID
	}
	pChain := platformvm.NewClient(environment.PChainAPI)
	height, err := pChain.GetHeight(ctx)
	if err != nil {
		return fmt.Errorf("read P-chain height: %w", err)
	}
	chainValidators, err := fetchValidatorsAt(ctx, pChain, targetSubnetID, height)
	if err != nil {
		return fmt.Errorf("read %s validators: %w", chainName, err)
	}
	target, err := findTarget(chainValidators, targetNodeID)
	if err != nil {
		return fmt.Errorf("identity %s: %w", identityName, err)
	}
	if target.Balance == 0 {
		return fmt.Errorf("identity %s validator %s is inactive", identityName, target.NodeID)
	}
	if target.Weight == targetWeight {
		fmt.Fprintf(output, "identity %s %s: already weight %d\n", identityName, target.NodeID, targetWeight)
		return nil
	}

	networkID, err := constants.NetworkID(environment.Network)
	if err != nil {
		return fmt.Errorf("resolve network %q: %w", environment.Network, err)
	}
	managementValidators, err := fetchValidatorsAt(ctx, pChain, deployment.ManagementSubnetID, height)
	if err != nil {
		return fmt.Errorf("read management validators: %w", err)
	}
	signers, err := loadManagerSigners(filepath.Join(deploymentDirectory, "manager"))
	if err != nil {
		return err
	}
	if len(managementValidators) != len(signers) {
		return fmt.Errorf("management validator count is %d, but %d local manager identities exist", len(managementValidators), len(signers))
	}
	conversionTime, err := managerConversionTime(managementValidators)
	if err != nil {
		return err
	}
	conversionHeight, err := findConversionHeight(
		ctx,
		platformBlockReader{client: pChain},
		height,
		conversionTime,
		deployment.ManagementConvertTxID,
	)
	if err != nil {
		return fmt.Errorf("locate management conversion transaction %s: %w", deployment.ManagementConvertTxID, err)
	}
	if err := gateManagementConversion(
		ctx,
		proposervm.NewJSONRPCClient(environment.PChainAPI, "P"),
		conversionHeight,
		upgrade.GetConfig(networkID).GraniteEpochDuration,
		time.Now(),
		wait,
		func() (ids.ID, error) {
			pWallet, err := makeFundingWallet(ctx, environment)
			if err != nil {
				return ids.Empty, err
			}
			tx, err := pWallet.IssueBaseTx(nil, commonopts.WithPollFrequency(time.Second))
			if err != nil {
				return ids.Empty, err
			}
			return tx.ID(), nil
		},
		output,
	); err != nil {
		return err
	}

	warpSet, err := canonicalWarpSet(managementValidators)
	if err != nil {
		return fmt.Errorf("build management signing set: %w", err)
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

	pWallet, err := makeFundingWallet(ctx, environment)
	if err != nil {
		return err
	}
	return submitAndVerify(
		ctx,
		pChain,
		pWallet,
		signed,
		identityName,
		target,
		targetWeight,
		output,
	)
}

func (r platformBlockReader) Block(ctx context.Context, height uint64) (time.Time, []ids.ID, error) {
	blockBytes, err := r.client.GetBlockByHeight(ctx, height)
	if err != nil {
		return time.Time{}, nil, err
	}
	parsed, err := pblock.Parse(pblock.Codec, blockBytes)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("parse P-chain block %d: %w", height, err)
	}
	banff, ok := parsed.(pblock.BanffBlock)
	if !ok {
		return time.Time{}, nil, nil
	}
	txs := parsed.Txs()
	txIDs := make([]ids.ID, len(txs))
	for i, tx := range txs {
		txIDs[i] = tx.ID()
	}
	return banff.Timestamp(), txIDs, nil
}

func managerConversionTime(validators []registeredValidator) (time.Time, error) {
	if len(validators) == 0 {
		return time.Time{}, fmt.Errorf("management L1 has no validators")
	}
	startTime := validators[0].StartTime
	if startTime == 0 {
		return time.Time{}, fmt.Errorf("management validator %s has no start time", validators[0].NodeID)
	}
	for _, validator := range validators[1:] {
		if validator.StartTime != startTime {
			return time.Time{}, fmt.Errorf("management validators have different conversion times: %d and %d", startTime, validator.StartTime)
		}
	}
	return time.Unix(int64(startTime), 0), nil
}

func findConversionHeight(
	ctx context.Context,
	blocks blockReader,
	tipHeight uint64,
	conversionTime time.Time,
	conversionTxID ids.ID,
) (uint64, error) {
	// L1 validator StartTime is the conversion block timestamp. P-chain block
	// timestamps are monotonic, so binary search reaches the first block stamped
	// at that second. The transaction ID then identifies the exact block when
	// several blocks share a timestamp. This derives one public-chain fact from
	// two other public-chain facts and deliberately stores no duplicate state.
	low, high := uint64(0), tipHeight
	for low < high {
		mid := low + (high-low)/2
		timestamp, _, err := blocks.Block(ctx, mid)
		if err != nil {
			return 0, fmt.Errorf("read P-chain block %d: %w", mid, err)
		}
		if timestamp.IsZero() || timestamp.Before(conversionTime) {
			low = mid + 1
		} else {
			high = mid
		}
	}

	for height := low; ; height++ {
		timestamp, txIDs, err := blocks.Block(ctx, height)
		if err != nil {
			return 0, fmt.Errorf("read P-chain block %d: %w", height, err)
		}
		if !timestamp.Equal(conversionTime) {
			break
		}
		for _, txID := range txIDs {
			if txID == conversionTxID {
				return height, nil
			}
		}
		if height == tipHeight {
			break
		}
	}
	return 0, fmt.Errorf("transaction was not found in P-chain blocks stamped %s", conversionTime.In(jst).Format("2006-01-02 15:04:05 MST"))
}

func makeFundingWallet(ctx context.Context, environment config.Environment) (pwallet.Wallet, error) {
	fundingKey, err := funding.ParsePrivateKey(environment.FundingPrivateKey)
	if err != nil {
		return nil, err
	}
	pWallet, err := primary.MakePWallet(
		ctx,
		environment.PChainAPI,
		secp256k1fx.NewKeychain(fundingKey),
		primary.WalletConfig{},
	)
	if err != nil {
		return nil, fmt.Errorf("connect P-chain wallet to %s: %w", environment.PChainAPI, err)
	}
	return pWallet, nil
}

func gateManagementConversion(
	ctx context.Context,
	epochs epochClient,
	conversionHeight uint64,
	epochDuration time.Duration,
	now time.Time,
	wait func(context.Context, time.Duration) error,
	nudge func() (ids.ID, error),
	output io.Writer,
) error {
	epoch, err := epochs.GetCurrentEpoch(ctx)
	if err != nil {
		return fmt.Errorf("read current P-chain Warp epoch: %w", err)
	}

	// ACP-181 deliberately freezes Warp's validator-set view at the epoch's
	// PChainHeight while normal P-chain blocks continue to advance. Therefore
	// current validator reads can include the management conversion while Warp
	// admission still sees an empty management set. Comparing the pinned height
	// with the conversion's exact inclusion height proves readiness directly;
	// epoch numbers and timestamps are only indirect signals.
	if epoch.PChainHeight >= conversionHeight {
		return nil
	}

	sealTime := time.Unix(epoch.StartTime, 0).Add(epochDuration)
	if now.Before(sealTime) {
		remaining := sealTime.Sub(now)
		displayRemaining := ((remaining + time.Second - 1) / time.Second) * time.Second
		fmt.Fprintf(
			output,
			"management conversion is not visible to Warp yet; sleeping for %s until %s\n",
			displayRemaining,
			sealTime.In(jst).Format("2006-01-02 15:04:05 MST"),
		)
		if err := wait(ctx, remaining); err != nil {
			return fmt.Errorf("wait for Warp epoch %d to become sealable: %w", epoch.Number, err)
		}
	}

	// A block whose parent predates the boundary cannot seal the epoch. A quiet
	// P-chain can therefore need two blocks after the wait: one to establish a
	// post-boundary parent and its child to start the new epoch. Recheck after
	// each visible no-op transaction and stop as soon as the conversion height
	// is pinned. This is a readiness wait, not a retry of the weight transaction,
	// which has not been constructed or submitted yet.
	for range 2 {
		nudgeID, err := nudge()
		if err != nil {
			return fmt.Errorf(
				"management conversion at P-chain height %d is not visible to Warp epoch %d pinned at height %d; issue P-chain context nudge: %w",
				conversionHeight,
				epoch.Number,
				epoch.PChainHeight,
				err,
			)
		}
		fmt.Fprintf(output, "issued P-chain Warp context nudge, tx %s\n", nudgeID)

		epoch, err = epochs.GetCurrentEpoch(ctx)
		if err != nil {
			return fmt.Errorf("read current P-chain Warp epoch after nudge %s: %w", nudgeID, err)
		}
		if epoch.PChainHeight >= conversionHeight {
			return nil
		}
	}

	return fmt.Errorf(
		"management conversion at P-chain height %d is still not visible to Warp epoch %d pinned at height %d after two P-chain context nudges",
		conversionHeight,
		epoch.Number,
		epoch.PChainHeight,
	)
}

func wait(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var jst = time.FixedZone("JST", 9*60*60)

func ValidateWeight(weight uint64) error {
	switch weight {
	case DeadWeight, SpareWeight, ActiveWeight:
		return nil
	default:
		return fmt.Errorf("set-weight weight must be 1, 1000, or 100000, got %d", weight)
	}
}

func validateIdentity(nodes []creation.PublicNode, name string) (creation.PublicNode, error) {
	index, err := identity.Index(name)
	if err != nil {
		return creation.PublicNode{}, err
	}
	if index >= len(nodes) {
		return creation.PublicNode{}, fmt.Errorf("identity %s is not present in public.json", name)
	}
	if nodes[index].Role != config.RoleValidator {
		return creation.PublicNode{}, fmt.Errorf("identity %s is an rpc identity; set-weight accepts validators only", name)
	}
	return nodes[index], nil
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
			StartTime:    state.StartTime,
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

func loadManagerSigners(directory string) ([]bls.Signer, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read management identities %s: %w", directory, err)
	}
	signers := make([]bls.Signer, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			return nil, fmt.Errorf("unexpected file in management identities: %s", filepath.Join(directory, entry.Name()))
		}
		if _, err := identity.Index(entry.Name()); err != nil {
			return nil, fmt.Errorf("management identity directory: %w", err)
		}
		path := filepath.Join(directory, entry.Name(), "signer.key")
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
	if len(signers) == 0 {
		return nil, fmt.Errorf("no management identities found in %s", directory)
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
	identityName string,
	validator registeredValidator,
	targetWeight uint64,
	output io.Writer,
) error {
	tx, err := pWallet.IssueSetL1ValidatorWeightTx(
		message.Bytes(),
		commonopts.WithPollFrequency(time.Second),
	)
	if err != nil {
		return fmt.Errorf("set identity %s validator %s weight %d -> %d: %w", identityName, validator.NodeID, validator.Weight, targetWeight, err)
	}
	fmt.Fprintf(output, "identity %s %s: weight %d -> %d, tx %s\n", identityName, validator.NodeID, validator.Weight, targetWeight, tx.ID())

	deadline := time.Now().Add(verifyTimeout)
	for {
		state, _, readErr := pChain.GetL1Validator(ctx, validator.ValidationID)
		if readErr == nil && state.Weight == targetWeight {
			fmt.Fprintf(output, "identity %s %s: verified weight %d\n", identityName, validator.NodeID, targetWeight)
			return nil
		}
		if time.Now().After(deadline) {
			if readErr != nil {
				return fmt.Errorf("verify identity %s weight: %w", identityName, readErr)
			}
			return fmt.Errorf("verify identity %s weight: still %d, expected %d after %s", identityName, state.Weight, targetWeight, verifyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
