// Package valmgr drives the stock icm-contracts v2 ValidatorManager deployed
// on the Fuji C-chain: deploy + initialize at create-l1 time, then weight-only
// updates from the reconcile loop (validators are registered once, at
// conversion, and never re-registered).
//
// The ABI and deploy bytecode in ValidatorManager.abi.json / .bin.hex are
// extracted verbatim from the generated Go binding in
// github.com/ava-labs/icm-services abi-bindings/go/validator-manager
// (commit 9c1e31be6). We embed the artifacts instead of importing the module
// because icm-services pins a much newer avalanchego and would drag ours up.
package valmgr

import (
	"context"
	"crypto/ecdsa"
	_ "embed"
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/crypto/secp256k1"

	"github.com/ava-labs/libevm/accounts/abi"
	"github.com/ava-labs/libevm/common"
	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/crypto"
	"github.com/ava-labs/libevm/ethclient"
)

//go:embed ValidatorManager.abi.json
var abiJSON string

// binHex is the UNLINKED ValidatorManager deploy bytecode: it contains a
// solc library placeholder (__$...$__) for the ValidatorMessages library,
// which must be deployed first and its address substituted in (exactly what
// the generated abigen Deploy does).
//
//go:embed ValidatorManager.bin.hex
var binHex string

//go:embed ValidatorMessages.bin.hex
var libBinHex string

// libPlaceholder matches solc library placeholders in unlinked bytecode.
var libPlaceholder = regexp.MustCompile(`__\$[0-9a-f]+\$__`)

const (
	// The three weight tiers a registered validator ever holds. The operator
	// sets a slot to one of them (`fleet weight validator|spare|dead`); the
	// seesaw ratchets the on-chain weight there.
	//
	//   ValidatorWeight - acting consensus member of the active DC.
	//   SpareWeight     - alive but idle: the standby DC. At 1% of an active
	//                     peer it holds real (nonzero) proposer-slot
	//                     probability, so it is a live consensus participant the
	//                     set can be moved onto WITHOUT the post-Durango proposer
	//                     deadlock a weight-1 standby would hit.
	//   DeadWeight      - stake pulled out of quorum: a failed box goes here so
	//                     its unreachable weight stops blocking finalization.
	//
	// spare:validator is 1:100 (was 1:1000), which cuts the post-failover
	// proposer-slot wait roughly 10x while dead weight stays negligible. A
	// healthy DC of 4 validators carries 400,000; the standby DC's spares add
	// ~4,000, so the active DC still holds ~99% of the weight and finalizes
	// every block on its own votes (nothing waits on the other DC). Totals
	// stay far below any uint64 product bound in the pipeline, and the 20%
	// churn-cap ratchet has ample room.
	ValidatorWeight uint64 = 100_000
	SpareWeight     uint64 = 1_000
	DeadWeight      uint64 = 1

	// Churn settings baked at initialize (write-once). Period 0 turns the rate
	// limit into a per-op size cap of maximumChurnPercentage of the running
	// total, which is what makes the weight seesaw possible without forking.
	churnPeriodSeconds     uint64 = 0
	maximumChurnPercentage uint8  = 20

	// Fixed gas for txs carrying a warp predicate: eth_estimateGas cannot
	// simulate predicate verification, so estimation would revert. Same value
	// icm-contracts' own tooling uses.
	warpTxGas = 2_000_000

	receiptTimeout = 3 * time.Minute
)

// warpPrecompile is the subnet-evm/coreth warp precompile address; a signed
// message is delivered to a contract by packing it into the access list under
// this address (predicate).
var warpPrecompile = common.HexToAddress("0x0200000000000000000000000000000000000005")

// Client is a thin wrapper over an EVM client + the ValidatorManager ABI.
type Client struct {
	eth     *ethclient.Client
	abi     abi.ABI
	key     *ecdsa.PrivateKey
	sender  common.Address
	chainID *big.Int
	Manager common.Address // contract address; zero until deployed or configured
}

// Dial connects to a C-chain RPC and prepares a transactor for the wallet key.
// manager may be the zero address when the contract is not deployed yet.
func Dial(ctx context.Context, rpcURL string, key *secp256k1.PrivateKey, manager common.Address) (*Client, error) {
	eth, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", rpcURL, err)
	}
	chainID, err := eth.ChainID(ctx)
	if err != nil {
		return nil, fmt.Errorf("chain id: %w", err)
	}
	parsed, err := abi.JSON(strings.NewReader(abiJSON))
	if err != nil {
		return nil, fmt.Errorf("parse embedded ABI: %w", err)
	}
	ecdsaKey := key.ToECDSA()
	return &Client{
		eth:     eth,
		abi:     parsed,
		key:     ecdsaKey,
		sender:  crypto.PubkeyToAddress(ecdsaKey.PublicKey),
		chainID: chainID,
		Manager: manager,
	}, nil
}

func (c *Client) Sender() common.Address { return c.sender }

// Balance returns the sender's C-chain balance in wei.
func (c *Client) Balance(ctx context.Context) (*big.Int, error) {
	return c.eth.BalanceAt(ctx, c.sender, nil)
}

// Deploy deploys the ValidatorMessages library, links its address into the
// ValidatorManager bytecode, and deploys the manager directly (no proxy;
// constructor arg 0 = ICMInitializable.Allowed so initialize is callable on
// the implementation itself). Returns the manager address.
func (c *Client) Deploy(ctx context.Context) (common.Address, error) {
	libBytecode, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(libBinHex), "0x"))
	if err != nil {
		return common.Address{}, fmt.Errorf("decode embedded library bytecode: %w", err)
	}
	libReceipt, err := c.transact(ctx, nil, libBytecode, nil, 0)
	if err != nil {
		return common.Address{}, fmt.Errorf("deploy ValidatorMessages library: %w", err)
	}

	linked := libPlaceholder.ReplaceAllString(strings.TrimSpace(binHex),
		strings.ToLower(libReceipt.ContractAddress.Hex()[2:]))
	bytecode, err := hex.DecodeString(strings.TrimPrefix(linked, "0x"))
	if err != nil {
		return common.Address{}, fmt.Errorf("decode linked bytecode: %w", err)
	}
	ctorArgs, err := c.abi.Pack("", uint8(0)) // ICMInitializable.Allowed
	if err != nil {
		return common.Address{}, fmt.Errorf("pack constructor: %w", err)
	}
	receipt, err := c.transact(ctx, nil, append(bytecode, ctorArgs...), nil, 0)
	if err != nil {
		return common.Address{}, fmt.Errorf("deploy: %w", err)
	}
	c.Manager = receipt.ContractAddress
	return receipt.ContractAddress, nil
}

// abiSettings mirrors struct ValidatorManagerSettings (field names must match
// the ABI component names CamelCased for go-ethereum's reflection packing).
type abiSettings struct {
	Admin                  common.Address
	SubnetID               [32]byte
	ChurnPeriodSeconds     uint64
	MaximumChurnPercentage uint8
}

// Initialize calls initialize(settings) with the churn-neutralizing settings.
// Callable exactly once per deployment.
func (c *Client) Initialize(ctx context.Context, admin common.Address, subnetID ids.ID) error {
	data, err := c.abi.Pack("initialize", abiSettings{
		Admin:                  admin,
		SubnetID:               subnetID,
		ChurnPeriodSeconds:     churnPeriodSeconds,
		MaximumChurnPercentage: maximumChurnPercentage,
	})
	if err != nil {
		return fmt.Errorf("pack initialize: %w", err)
	}
	_, err = c.transact(ctx, &c.Manager, data, nil, 0)
	return err
}

type abiInitialValidator struct {
	NodeID       []byte
	BlsPublicKey []byte
	Weight       uint64
}

type abiConversionData struct {
	SubnetID                     [32]byte
	ValidatorManagerBlockchainID [32]byte
	ValidatorManagerAddress      common.Address
	InitialValidators            []abiInitialValidator
}

// InitialValidator is one conversion-time validator as the contract sees it.
type InitialValidator struct {
	NodeID       []byte // 20-byte NodeID
	BLSPublicKey []byte // 48-byte compressed BLS key
	Weight       uint64
}

// InitializeValidatorSet delivers the P-chain-signed SubnetToL1Conversion
// message (predicate at index 0) and replays the exact ConversionData that was
// hashed at conversion. The contract recomputes the hash, so every byte must
// match the ConvertSubnetToL1Tx.
func (c *Client) InitializeValidatorSet(
	ctx context.Context,
	subnetID ids.ID,
	managerChainID ids.ID,
	validators []InitialValidator,
	signedConversionMsg []byte,
) error {
	conv := abiConversionData{
		SubnetID:                     subnetID,
		ValidatorManagerBlockchainID: managerChainID,
		ValidatorManagerAddress:      c.Manager,
		InitialValidators:            make([]abiInitialValidator, len(validators)),
	}
	for i, v := range validators {
		conv.InitialValidators[i] = abiInitialValidator{
			NodeID:       v.NodeID,
			BlsPublicKey: v.BLSPublicKey,
			Weight:       v.Weight,
		}
	}
	data, err := c.abi.Pack("initializeValidatorSet", conv, uint32(0))
	if err != nil {
		return fmt.Errorf("pack initializeValidatorSet: %w", err)
	}
	_, err = c.transact(ctx, &c.Manager, data, predicateAccessList(signedConversionMsg), warpTxGas)
	return err
}

// WeightStep is one pre-computed initiateValidatorWeightUpdate call.
type WeightStep struct {
	ValidationID ids.ID
	Weight       uint64
}

// InitiateWeightUpdates fires every step in one burst with consecutive nonces,
// no receipt wait in between, so the whole ratchet lands in one or two blocks
// instead of one block per step. Same-sender txs execute in nonce order, and
// the churn tracker (period 0) re-seeds per call even within a block, so the
// on-chain result matches the caller's local simulation. Receipts are checked
// after everything is sent; the emitted warp messages are reconstructed
// deterministically later (WeightMessage), nothing is scraped from receipts.
func (c *Client) InitiateWeightUpdates(ctx context.Context, steps []WeightStep) error {
	if len(steps) == 0 {
		return nil
	}
	nonce, err := c.eth.PendingNonceAt(ctx, c.sender)
	if err != nil {
		return fmt.Errorf("nonce: %w", err)
	}
	hashes := make([]common.Hash, len(steps))
	for i, s := range steps {
		data, err := c.abi.Pack("initiateValidatorWeightUpdate", [32]byte(s.ValidationID), s.Weight)
		if err != nil {
			return fmt.Errorf("pack initiateValidatorWeightUpdate: %w", err)
		}
		// Fixed gas: estimating step i>0 against the pre-batch state would
		// falsely revert on the churn check. Unused gas is refunded.
		h, err := c.sendTx(ctx, nonce+uint64(i), warpTxGas, &c.Manager, data, nil)
		if err != nil {
			return fmt.Errorf("send initiate %d/%d: %w", i+1, len(steps), err)
		}
		hashes[i] = h
	}
	for i, h := range hashes {
		if _, err := c.waitMined(ctx, h); err != nil {
			return fmt.Errorf("initiate %d/%d: %w", i+1, len(steps), err)
		}
	}
	return nil
}

// Validator mirrors the contract's Validator struct (getValidator return).
type Validator struct {
	Status         uint8
	NodeID         []byte
	StartingWeight uint64
	SentNonce      uint64
	ReceivedNonce  uint64
	Weight         uint64
	StartTime      uint64
	EndTime        uint64
}

// Active is the ValidatorStatus enum value for a live validator.
const StatusActive uint8 = 2

func (c *Client) GetValidator(ctx context.Context, validationID ids.ID) (Validator, error) {
	var v Validator
	out, err := c.view(ctx, "getValidator", [32]byte(validationID))
	if err != nil {
		return v, err
	}
	v = *abi.ConvertType(out[0], new(Validator)).(*Validator)
	return v, nil
}

func (c *Client) L1TotalWeight(ctx context.Context) (uint64, error) {
	out, err := c.view(ctx, "l1TotalWeight")
	if err != nil {
		return 0, err
	}
	return out[0].(uint64), nil
}

func (c *Client) IsValidatorSetInitialized(ctx context.Context) (bool, error) {
	out, err := c.view(ctx, "isValidatorSetInitialized")
	if err != nil {
		return false, err
	}
	return out[0].(bool), nil
}

func (c *Client) Owner(ctx context.Context) (common.Address, error) {
	out, err := c.view(ctx, "owner")
	if err != nil {
		return common.Address{}, err
	}
	return out[0].(common.Address), nil
}

func (c *Client) ChurnPeriodSeconds(ctx context.Context) (uint64, error) {
	out, err := c.view(ctx, "getChurnPeriodSeconds")
	if err != nil {
		return 0, err
	}
	return out[0].(uint64), nil
}

func (c *Client) view(ctx context.Context, method string, args ...any) ([]any, error) {
	data, err := c.abi.Pack(method, args...)
	if err != nil {
		return nil, fmt.Errorf("pack %s: %w", method, err)
	}
	raw, err := c.eth.CallContract(ctx, callMsg(c.Manager, data), nil)
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", method, err)
	}
	out, err := c.abi.Unpack(method, raw)
	if err != nil {
		return nil, fmt.Errorf("unpack %s: %w", method, err)
	}
	return out, nil
}

// transact builds, signs, sends a DynamicFeeTx and waits for its receipt.
// gas 0 means estimate (only valid for predicate-free txs).
func (c *Client) transact(
	ctx context.Context,
	to *common.Address,
	data []byte,
	accessList types.AccessList,
	gas uint64,
) (*types.Receipt, error) {
	nonce, err := c.eth.PendingNonceAt(ctx, c.sender)
	if err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	if gas == 0 {
		msg := callMsg(common.Address{}, data)
		msg.To = to
		msg.From = c.sender
		gas, err = c.eth.EstimateGas(ctx, msg)
		if err != nil {
			return nil, fmt.Errorf("estimate gas: %w", err)
		}
		gas += gas / 2 // headroom; unused gas is refunded
	}
	h, err := c.sendTx(ctx, nonce, gas, to, data, accessList)
	if err != nil {
		return nil, err
	}
	return c.waitMined(ctx, h)
}

// sendTx signs and broadcasts one DynamicFeeTx at an explicit nonce without
// waiting for it to mine.
func (c *Client) sendTx(
	ctx context.Context,
	nonce, gas uint64,
	to *common.Address,
	data []byte,
	accessList types.AccessList,
) (common.Hash, error) {
	tip, err := c.eth.SuggestGasTipCap(ctx)
	if err != nil {
		return common.Hash{}, fmt.Errorf("tip cap: %w", err)
	}
	head, err := c.eth.HeaderByNumber(ctx, nil)
	if err != nil {
		return common.Hash{}, fmt.Errorf("head: %w", err)
	}
	feeCap := new(big.Int).Add(tip, new(big.Int).Mul(head.BaseFee, big.NewInt(2)))
	tx := types.NewTx(&types.DynamicFeeTx{
		ChainID:    c.chainID,
		Nonce:      nonce,
		To:         to,
		Gas:        gas,
		GasFeeCap:  feeCap,
		GasTipCap:  tip,
		Value:      common.Big0,
		Data:       data,
		AccessList: accessList,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(c.chainID), c.key)
	if err != nil {
		return common.Hash{}, fmt.Errorf("sign: %w", err)
	}
	if err := c.eth.SendTransaction(ctx, signed); err != nil {
		return common.Hash{}, fmt.Errorf("send: %w", err)
	}
	return signed.Hash(), nil
}

func (c *Client) waitMined(ctx context.Context, h common.Hash) (*types.Receipt, error) {
	deadline := time.Now().Add(receiptTimeout)
	for {
		receipt, err := c.eth.TransactionReceipt(ctx, h)
		if err == nil {
			if receipt.Status != types.ReceiptStatusSuccessful {
				return receipt, fmt.Errorf("tx %s reverted", h)
			}
			return receipt, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("tx %s not mined within %s", h, receiptTimeout)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}
