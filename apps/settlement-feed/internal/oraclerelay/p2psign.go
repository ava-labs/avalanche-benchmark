package oraclerelay

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/message"
	"github.com/ava-labs/avalanchego/network/p2p"
	"github.com/ava-labs/avalanchego/network/p2p/acp118"
	"github.com/ava-labs/avalanchego/network/peer"
	p2ppb "github.com/ava-labs/avalanchego/proto/pb/p2p"
	"github.com/ava-labs/avalanchego/proto/pb/sdk"
	"github.com/ava-labs/avalanchego/snow/validators"
	"github.com/ava-labs/avalanchego/utils/compression"
	"github.com/ava-labs/avalanchego/utils/crypto/bls"
	"github.com/ava-labs/avalanchego/utils/set"
	"github.com/ava-labs/avalanchego/vms/platformvm/warp"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"
)

// The relay collects signatures the way icm-services' signature-aggregator
// does: ACP-118 SignatureRequest AppRequests over each validator's staking
// port. The relay holds no BLS keys; validators sign their own Warp messages.
// The fleet's validators are known statically from the inventory, so no peer
// discovery is needed: dial each one once at startup and hold the connection.
const (
	// p2pSignTimeout bounds one message's signature collection. A validator
	// signs from its in-memory Warp backend, so the budget is round-trip
	// dominated; anything slower than this means the fleet is unhealthy.
	p2pSignTimeout = 2 * time.Second
	// p2pDialTimeout bounds the one-time startup dial+handshake per validator.
	p2pDialTimeout = 15 * time.Second
)

// signatureReply is one validator's answer to a SignatureRequest.
type signatureReply struct {
	nodeID       ids.NodeID
	responseData []byte // marshaled sdk.SignatureResponse; nil when the node returned an error
	errorMessage string
}

// responseMux routes inbound AppResponse/AppError messages from every peer
// connection back to the Sign call that issued the matching requestID.
type responseMux struct {
	mu      sync.Mutex
	waiters map[uint32]chan<- signatureReply
}

func newResponseMux() *responseMux {
	return &responseMux{waiters: make(map[uint32]chan<- signatureReply)}
}

func (m *responseMux) expect(requestID uint32, replies chan<- signatureReply) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.waiters[requestID] = replies
}

func (m *responseMux) forget(requestID uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.waiters, requestID)
}

func (m *responseMux) deliver(requestID uint32, reply signatureReply) {
	m.mu.Lock()
	replies, ok := m.waiters[requestID]
	delete(m.waiters, requestID)
	m.mu.Unlock()
	if ok {
		replies <- reply
	}
}

// HandleInbound implements router.InboundHandler over the raw peer connection.
// Everything except signature responses (pings, gossip) is discarded.
func (m *responseMux) HandleInbound(_ context.Context, msg *message.InboundMessage) {
	defer msg.OnFinishedHandling()
	switch payload := msg.Message.(type) {
	case *p2ppb.AppResponse:
		m.deliver(payload.RequestId, signatureReply{nodeID: msg.NodeID, responseData: payload.AppBytes})
	case *p2ppb.AppError:
		m.deliver(payload.RequestId, signatureReply{nodeID: msg.NodeID, errorMessage: payload.ErrorMessage})
	}
}

// p2pSigner requests each oracle validator's BLS signature over p2p and
// aggregates replies into a BitSetSignature once the quorum weight is reached.
type p2pSigner struct {
	peers         []*peer.Peer
	creator       message.Creator
	chainID       ids.ID
	warpSet       validators.WarpSet
	indexByNodeID map[ids.NodeID]int
	mux           *responseMux
	nextRequestID atomic.Uint32
}

// newP2PSigner dials every staking address, completes the TLS handshake, and
// verifies each connected node is an oracle canonical-set validator. The
// signature request payload is routed to chainID's ACP-118 handler.
func newP2PSigner(
	ctx context.Context,
	stakingAddresses []netip.AddrPort,
	networkID uint32,
	chainID ids.ID,
	warpSet validators.WarpSet,
	output io.Writer,
) (*p2pSigner, error) {
	creator, err := message.NewCreator(prometheus.NewRegistry(), compression.TypeZstd, 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("build p2p message creator: %w", err)
	}
	indexByNodeID := make(map[ids.NodeID]int)
	for index, validator := range warpSet.Validators {
		for _, nodeID := range validator.NodeIDs {
			indexByNodeID[nodeID] = index
		}
	}
	signer := &p2pSigner{
		creator:       creator,
		chainID:       chainID,
		warpSet:       warpSet,
		indexByNodeID: indexByNodeID,
		mux:           newResponseMux(),
	}
	for _, address := range stakingAddresses {
		dialCtx, cancel := context.WithTimeout(ctx, p2pDialTimeout)
		connected, err := peer.StartTestPeer(dialCtx, address, networkID, signer.mux)
		cancel()
		if err != nil {
			signer.Close()
			return nil, fmt.Errorf("dial oracle validator staking port %s: %w", address, err)
		}
		if _, ok := indexByNodeID[connected.ID()]; !ok {
			signer.Close()
			return nil, fmt.Errorf("node %s at %s is not in the oracle canonical validator set", connected.ID(), address)
		}
		signer.peers = append(signer.peers, connected)
		fmt.Fprintf(output, "p2p signer connected to oracle validator %s at %s\n", connected.ID(), address)
	}
	return signer, nil
}

// Close tears down every peer connection.
func (s *p2pSigner) Close() {
	for _, connected := range s.peers {
		connected.StartClose()
	}
}

// Sign requests a signature from every connected validator concurrently and
// returns as soon as replies reach the Warp quorum, exactly as the production
// signature-aggregator does. Late replies are dropped by the mux.
func (s *p2pSigner) Sign(ctx context.Context, unsigned *warp.UnsignedMessage) (*warp.Message, error) {
	requestBytes, err := proto.Marshal(&sdk.SignatureRequest{Message: unsigned.Bytes()})
	if err != nil {
		return nil, fmt.Errorf("marshal signature request: %w", err)
	}
	payload := p2p.PrefixMessage(p2p.ProtocolPrefix(acp118.HandlerID), requestBytes)

	replies := make(chan signatureReply, len(s.peers))
	requestIDs := make([]uint32, 0, len(s.peers))
	outstanding := 0
	for _, connected := range s.peers {
		// Odd requestIDs only: subnet-evm splits the inbound AppRequest
		// requestID space by parity: even IDs go to its legacy sync-handler
		// network, which silently drops ACP-118 payloads; odd IDs reach the SDK
		// router that owns the signature handler. Proven empirically: even IDs
		// time out, odd IDs answer in under a millisecond.
		requestID := s.nextRequestID.Add(2) | 1
		outbound, err := s.creator.AppRequest(s.chainID, requestID, p2pSignTimeout, payload)
		if err != nil {
			return nil, fmt.Errorf("build signature AppRequest: %w", err)
		}
		s.mux.expect(requestID, replies)
		requestIDs = append(requestIDs, requestID)
		if !connected.Send(ctx, outbound) {
			s.mux.forget(requestID)
			continue
		}
		outstanding++
	}
	defer func() {
		for _, requestID := range requestIDs {
			s.mux.forget(requestID)
		}
	}()

	signatures := make(map[int]*bls.Signature, len(s.peers))
	var signedWeight uint64
	timeout := time.NewTimer(p2pSignTimeout)
	defer timeout.Stop()
	for signedWeight*quorumDenominator < s.warpSet.TotalWeight*quorumNumerator {
		if outstanding == 0 {
			return nil, fmt.Errorf("p2p signature quorum unreachable: %d/%d weight signed and no replies outstanding", signedWeight, s.warpSet.TotalWeight)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, fmt.Errorf("p2p signature quorum timeout after %s: %d/%d weight signed", p2pSignTimeout, signedWeight, s.warpSet.TotalWeight)
		case reply := <-replies:
			outstanding--
			index, ok := s.indexByNodeID[reply.nodeID]
			if !ok {
				continue
			}
			if reply.responseData == nil {
				return nil, fmt.Errorf("validator %s refused to sign: %s", reply.nodeID, reply.errorMessage)
			}
			if _, exists := signatures[index]; exists {
				continue
			}
			var response sdk.SignatureResponse
			if err := proto.Unmarshal(reply.responseData, &response); err != nil {
				return nil, fmt.Errorf("decode signature response from %s: %w", reply.nodeID, err)
			}
			signature, err := bls.SignatureFromBytes(response.Signature)
			if err != nil {
				return nil, fmt.Errorf("parse BLS signature from %s: %w", reply.nodeID, err)
			}
			validator := s.warpSet.Validators[index]
			if !bls.Verify(validator.PublicKey, signature, unsigned.Bytes()) {
				return nil, fmt.Errorf("validator %s returned a signature that does not verify against its registered key", reply.nodeID)
			}
			signatures[index] = signature
			signedWeight += validator.Weight
		}
	}
	return aggregateByIndex(unsigned, s.warpSet, signatures)
}

// aggregateByIndex builds the BitSetSignature from per-canonical-index
// signatures (replies arrive keyed by NodeID, mapped to canonical index),
// aggregates in canonical order, and verifies the quorum locally before
// returning so a bad aggregate fails here rather than on-chain.
func aggregateByIndex(unsigned *warp.UnsignedMessage, validatorSet validators.WarpSet, byIndex map[int]*bls.Signature) (*warp.Message, error) {
	indexes := make([]int, 0, len(byIndex))
	for index := range byIndex {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	signerBits := set.NewBits()
	signatures := make([]*bls.Signature, 0, len(indexes))
	for _, index := range indexes {
		signerBits.Add(index)
		signatures = append(signatures, byIndex[index])
	}
	aggregated, err := bls.AggregateSignatures(signatures)
	if err != nil {
		return nil, fmt.Errorf("aggregate Warp signatures: %w", err)
	}
	bitSetSignature := &warp.BitSetSignature{Signers: signerBits.Bytes()}
	copy(bitSetSignature.Signature[:], bls.SignatureToBytes(aggregated))
	if err := bitSetSignature.Verify(unsigned, unsigned.NetworkID, validatorSet, quorumNumerator, quorumDenominator); err != nil {
		return nil, fmt.Errorf("local oracle quorum verification: %w", err)
	}
	message, err := warp.NewMessage(unsigned, bitSetSignature)
	if err != nil {
		return nil, fmt.Errorf("build signed Warp message: %w", err)
	}
	return message, nil
}

// ParseStakingAddresses parses the comma-separated <ip:port> list given to
// `relay ... p2p=<...>`.
func ParseStakingAddresses(list string) ([]netip.AddrPort, error) {
	var addresses []netip.AddrPort
	for _, entry := range strings.Split(list, ",") {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		address, err := netip.ParseAddrPort(entry)
		if err != nil {
			return nil, fmt.Errorf("staking address %q: %w", entry, err)
		}
		addresses = append(addresses, address)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("p2p signing requires at least one <ip:port> staking address")
	}
	return addresses, nil
}
