package main

import (
	"fmt"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	warppayload "github.com/ava-labs/avalanchego/vms/platformvm/warp/payload"
)

// quorum is the P-chain's warp acceptance threshold (67/100, see
// avalanchego warp_verifier.go). We hold every validator key, so a local
// verify at exactly the protocol quorum is the same check the chain runs.
const (
	quorumNum = 67
	quorumDen = 100
)

// addressedCall wraps a warp message payload the way the P-chain expects it
// from an L1's validator manager: AddressedCall(sourceAddr=manager address)
// inside UnsignedMessage(networkID, sourceChainID=the L1's blockchainID).
func addressedCall(networkID uint32, sourceChainID ids.ID, sourceAddr []byte, payload []byte) (*warp.UnsignedMessage, error) {
	call, err := warppayload.NewAddressedCall(sourceAddr, payload)
	if err != nil {
		return nil, err
	}
	return warp.NewUnsignedMessage(networkID, sourceChainID, call.Bytes())
}

// signAndAggregate signs unsigned with every local signer whose public key is
// in the canonical validator set, aggregates the signatures into a
// warp.BitSetSignature and verifies the result at the protocol quorum before
// returning it. vdrs must be the canonical set (validators.FlattenValidatorSet
// output) for the L1's subnet.
func signAndAggregate(unsigned *warp.UnsignedMessage, vdrs validators.WarpSet, signers []bls.Signer) (*warp.Message, error) {
	byPK := make(map[string]bls.Signer, len(signers))
	for _, s := range signers {
		byPK[string(bls.PublicKeyToUncompressedBytes(s.PublicKey()))] = s
	}

	bits := set.NewBits()
	var sigs []*bls.Signature
	for i, v := range vdrs.Validators {
		s, ok := byPK[string(v.PublicKeyBytes)]
		if !ok {
			continue
		}
		sig, err := s.Sign(unsigned.Bytes())
		if err != nil {
			return nil, fmt.Errorf("sign: %w", err)
		}
		sigs = append(sigs, sig)
		bits.Add(i)
	}
	if len(sigs) == 0 {
		return nil, fmt.Errorf("none of the %d local keys are in the %d-validator canonical set", len(signers), len(vdrs.Validators))
	}

	agg, err := bls.AggregateSignatures(sigs)
	if err != nil {
		return nil, fmt.Errorf("aggregate signatures: %w", err)
	}
	bitSig := &warp.BitSetSignature{Signers: bits.Bytes()}
	copy(bitSig.Signature[:], bls.SignatureToBytes(agg))

	if err := bitSig.Verify(unsigned, unsigned.NetworkID, vdrs, quorumNum, quorumDen); err != nil {
		return nil, fmt.Errorf("local quorum verify (%d local keys over %d validators): %w", len(sigs), len(vdrs.Validators), err)
	}
	return warp.NewMessage(unsigned, bitSig)
}
