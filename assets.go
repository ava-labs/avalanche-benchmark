package benchmark

import (
	_ "embed"
	"fmt"
)

const (
	SubnetEVMID = "srEXiWaHuhNyGwPUi444Tu47ZEDwxTWrbQiuD7FmgSAQ6X7Dy"

	PChain1NodeID = "NodeID-7TTNrr8K83oCmEMTXsLxPZKdRUP9umNzo"
	PChain2NodeID = "NodeID-9cQcUqMVNrsFgifcuzDmqtQqRm8YHA3EG"

	L1Validator1NodeID = "NodeID-7Xhw2mDxuDS44j42TCB6U5579esbSt3Lg"
	L1Validator2NodeID = "NodeID-MFrZFVCXPv5iCn6M9K6XduxGTYp891xXZ"
	L1Validator3NodeID = "NodeID-NFBbbJ4qCmNaCzeW7sxErhvWqvEQMnYcN"
	L1Validator4NodeID = "NodeID-GWPcbFJZFfZreETSoWjPimr846mXEKCtu"
	L1Validator5NodeID = "NodeID-P7oB2McjBGgW2NXXWVYjV8JEDFoW9xDE5"
)

//go:embed staking/pchain/1/signer.key
var pchain1SignerKey []byte

//go:embed staking/pchain/1/staker.crt
var pchain1StakerCert []byte

//go:embed staking/pchain/1/staker.key
var pchain1StakerKey []byte

//go:embed staking/pchain/2/signer.key
var pchain2SignerKey []byte

//go:embed staking/pchain/2/staker.crt
var pchain2StakerCert []byte

//go:embed staking/pchain/2/staker.key
var pchain2StakerKey []byte

//go:embed staking/l1/1/signer.key
var l1Validator1SignerKey []byte

//go:embed staking/l1/2/signer.key
var l1Validator2SignerKey []byte

//go:embed staking/l1/3/signer.key
var l1Validator3SignerKey []byte

//go:embed staking/l1/4/signer.key
var l1Validator4SignerKey []byte

//go:embed staking/l1/5/signer.key
var l1Validator5SignerKey []byte

type StakingFile struct {
	Name string
	Data []byte
}

type L1ValidatorIdentity struct {
	Index     int
	NodeID    string
	SignerKey []byte
}

func PChainStakingFiles(index int) ([]StakingFile, error) {
	switch index {
	case 1:
		return []StakingFile{
			{Name: "signer.key", Data: pchain1SignerKey},
			{Name: "staker.crt", Data: pchain1StakerCert},
			{Name: "staker.key", Data: pchain1StakerKey},
		}, nil
	case 2:
		return []StakingFile{
			{Name: "signer.key", Data: pchain2SignerKey},
			{Name: "staker.crt", Data: pchain2StakerCert},
			{Name: "staker.key", Data: pchain2StakerKey},
		}, nil
	default:
		return nil, fmt.Errorf("unknown P-Chain staking key index %d", index)
	}
}

func PChainNodeID(index int) (string, error) {
	switch index {
	case 1:
		return PChain1NodeID, nil
	case 2:
		return PChain2NodeID, nil
	default:
		return "", fmt.Errorf("unknown P-Chain node ID index %d", index)
	}
}

func L1ValidatorIdentities(count int) ([]L1ValidatorIdentity, error) {
	if count < 1 || count > 5 {
		return nil, fmt.Errorf("L1 validator count must be between 1 and 5, got %d", count)
	}

	all := []L1ValidatorIdentity{
		{Index: 1, NodeID: L1Validator1NodeID, SignerKey: l1Validator1SignerKey},
		{Index: 2, NodeID: L1Validator2NodeID, SignerKey: l1Validator2SignerKey},
		{Index: 3, NodeID: L1Validator3NodeID, SignerKey: l1Validator3SignerKey},
		{Index: 4, NodeID: L1Validator4NodeID, SignerKey: l1Validator4SignerKey},
		{Index: 5, NodeID: L1Validator5NodeID, SignerKey: l1Validator5SignerKey},
	}
	return all[:count], nil
}
