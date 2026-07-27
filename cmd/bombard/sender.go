package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/rpc"
)

const (
	// sendConcPerNode is the number of sender goroutines (and thus the cap on
	// concurrent keep-alive connections) per node. Sized to sustain the target
	// rps to a single node at healthy latency with headroom; capped so we reuse
	// connections instead of churning ephemeral ports / hitting fd limits.
	sendConcPerNode = 64
	// sendQueueLen is the per-node buffered queue depth. A dead or slow node
	// fills its queue and then drops, it never blocks the issuer or other
	// nodes. Resubmission and the other nodes cover the dropped sends.
	sendQueueLen = 4096

	// Healthy ingress routing (mirrors a production load-balancer health check):
	// drop an endpoint from the send rotation once it falls ingressDropBehind
	// blocks behind the furthest-ahead endpoint, and re-add it once within
	// ingressRejoinWithin. Routing ingress to a node that isn't caught up wastes
	// sends and, worse, keeps a recovering node (e.g. a wiped RPC) from ever
	// catching up. Hysteresis (drop >> rejoin) avoids flapping at the boundary.
	ingressDropBehind    = 200
	ingressRejoinWithin  = 50
	ingressCheckInterval = 3 * time.Second
	// ingressAtTipWithin gates ISSUANCE (not just rotation): bombard only sends the
	// sequential-nonce stream to an endpoint within this many blocks of the tip.
	// "Healthy" (within ingressDropBehind) is the right bar for a load balancer, but
	// fatal for sequential nonces, a tx whose nonce sits above a behind endpoint's
	// accepted frontier is an UNFILLABLE GAP: the node queues it but cannot mine it
	// until it catches up, pinning throughput at 0. This bit is what keeps a graceful
	// restore from stalling bombard: when the validator majority (and thus the active
	// RPC set) flips to the recovering site, its archive RPCs are still catching up, // so we keep issuing to the at-tip site we are restoring FROM until the recovering
	// RPCs are genuinely at tip, not merely "in rotation". Well above normal under-load
	// lag, well below a restore catch-up backlog, so it neither flaps nor sends into a gap.
	ingressAtTipWithin = 25
)

// broadcaster sends every tx to every node over HTTP. Each node drains its own
// buffered queue with a fixed pool of sender goroutines, so a down or slow node
// only backs up (and drops) its own queue. All nodes share ONE keep-alive HTTP
// transport, so connections are reused rather than recreated per send.
type broadcaster struct {
	nodes   []*nodeSender
	timeout time.Duration
}

type nodeSender struct {
	url     string
	client  *ethclient.Client
	queue   chan *types.Transaction
	healthy atomic.Bool // in send rotation while within ingressDropBehind of tip
	atTip   atomic.Bool // within ingressAtTipWithin of tip, safe to issue nonces to
	active  atomic.Bool // routed ingress only while on the active (validator) site
}

// newBroadcaster dials every rpcURL (lazily, over HTTP, a down node is included
// and simply errors on send) behind a single tuned transport and starts the
// per-node sender pools. sendTimeout bounds each individual send.
func newBroadcaster(ctx context.Context, rpcURLs []string, sendTimeout time.Duration) (*broadcaster, error) {
	if len(rpcURLs) == 0 {
		return nil, fmt.Errorf("broadcaster needs at least one URL")
	}

	// One shared transport. Keep-alive connections are pooled per host, bounded
	// at sendConcPerNode so we never exceed what the sender goroutines can use, // no ephemeral-port churn, no fd blowup.
	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: sendTimeout, KeepAlive: 30 * time.Second}).DialContext,
		MaxConnsPerHost:     sendConcPerNode,
		MaxIdleConnsPerHost: sendConcPerNode,
		MaxIdleConns:        sendConcPerNode * len(rpcURLs),
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: sendTimeout,
	}
	httpClient := &http.Client{Transport: tr} // no client-level timeout: per-call ctx bounds each send

	b := &broadcaster{timeout: sendTimeout}
	for _, url := range rpcURLs {
		rc, err := rpc.DialOptions(ctx, url, rpc.WithHTTPClient(httpClient))
		if err != nil {
			// HTTP dial is lazy, so this is rare; skip a malformed URL but keep going.
			fmt.Printf("broadcaster: skipping unusable endpoint %s (%v)\n", url, err)
			continue
		}
		ns := &nodeSender{
			url:    url,
			client: ethclient.NewClient(rc),
			queue:  make(chan *types.Transaction, sendQueueLen),
		}
		ns.healthy.Store(true)
		ns.atTip.Store(true)  // assume at-tip until monitorIngress measures otherwise
		ns.active.Store(true) // all endpoints active until an active-rpcs file narrows it
		b.nodes = append(b.nodes, ns)
		for i := 0; i < sendConcPerNode; i++ {
			go ns.run(ctx, sendTimeout)
		}
	}
	if len(b.nodes) == 0 {
		return nil, fmt.Errorf("no usable endpoints among %d", len(rpcURLs))
	}
	go b.monitorIngress(ctx)
	return b, nil
}

// run drains the node's queue, sending each tx with a tight per-call timeout and
// ignoring all errors (already-known, nonce races, a down node, all expected).
func (n *nodeSender) run(ctx context.Context, timeout time.Duration) {
	for {
		select {
		case <-ctx.Done():
			return
		case tx := <-n.queue:
			sctx, cancel := context.WithTimeout(ctx, timeout)
			_ = n.client.SendTransaction(sctx, tx)
			cancel()
		}
	}
}

// broadcast enqueues signed to every node, non-blocking: if a node's queue is
// full (it is down or lagging) the send is dropped for that node only.
func (b *broadcaster) broadcast(signed *types.Transaction) {
	// Pick the send pool in priority order. Sequential-nonce issuance must land on an
	// endpoint AT THE TIP: a tx whose nonce is above a behind endpoint's accepted
	// frontier is an unfillable gap that pins throughput at 0 (see ingressAtTipWithin).
	//   1. active + at-tip, steady state, and the end state of a completed migration.
	//   2. any at-tip, covers the restore window: the active set has flipped to the
	//                        recovering site whose RPCs aren't caught up yet, so keep
	//                        issuing to the at-tip site we are restoring FROM.
	//   3. any healthy, nothing is fully at tip but something is in rotation; a brief
	//                        small-gap hop beats a stall.
	//   4. everything, nothing looks caught up at all; spray all so ingress never
	//                        hard-stops, and the resubmit loop retries as nodes recover.
	activeAtTip, anyAtTip, anyHealthy := false, false, false
	for _, n := range b.nodes {
		if n.atTip.Load() {
			anyAtTip = true
			if n.active.Load() {
				activeAtTip = true
			}
		}
		if n.healthy.Load() {
			anyHealthy = true
		}
	}
	eligible := func(n *nodeSender) bool {
		switch {
		case activeAtTip:
			return n.active.Load() && n.atTip.Load()
		case anyAtTip:
			return n.atTip.Load()
		case anyHealthy:
			return n.healthy.Load()
		default:
			return true
		}
	}
	for _, n := range b.nodes {
		if !eligible(n) {
			continue
		}
		select {
		case n.queue <- signed:
		default:
			// Node is saturated/down; drop. Other nodes still get it and the
			// resubmit loop will retry.
		}
	}
}

// monitorIngress polls every endpoint's height and routes ingress only to those
// caught up to the tip, mirroring a load-balancer health check. A node that has
// fallen behind (e.g. a freshly-wiped RPC rejoining mid-load) is taken out of the
// send rotation so it can catch up without also serving load, then re-added once
// within range. The furthest-ahead node is behind=0, so at least one endpoint is
// always healthy.
func (b *broadcaster) monitorIngress(ctx context.Context) {
	t := time.NewTicker(ingressCheckInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			heights := make([]uint64, len(b.nodes))
			var maxH uint64
			for i, n := range b.nodes {
				hctx, cancel := context.WithTimeout(ctx, 4*time.Second)
				h, err := n.client.BlockNumber(hctx)
				cancel()
				if err == nil {
					heights[i] = h
					if h > maxH {
						maxH = h
					}
				}
			}
			if maxH == 0 {
				continue // nothing reachable yet, leave routing unchanged
			}
			for i, n := range b.nodes {
				behind := maxH - heights[i]
				switch {
				case n.healthy.Load() && behind > ingressDropBehind:
					n.healthy.Store(false)
					fmt.Fprintf(os.Stderr, "ingress: %s out of rotation, %d blocks behind tip (catching up)\n", n.url, behind)
				case !n.healthy.Load() && behind <= ingressRejoinWithin:
					n.healthy.Store(true)
					fmt.Fprintf(os.Stderr, "ingress: %s back in rotation, caught up (%d behind)\n", n.url, behind)
				}
				// atTip is the stricter issuance gate (see ingressAtTipWithin). No
				// hysteresis: a few blocks of lag is normal under load, so flapping
				// across this boundary is harmless (it just shifts issuance between
				// equally-at-tip endpoints), and a restore backlog is far past it.
				atTip := behind <= ingressAtTipWithin
				if n.atTip.Swap(atTip) != atTip && !atTip {
					fmt.Fprintf(os.Stderr, "ingress: %s no longer at tip, %d behind (holding issuance off it)\n", n.url, behind)
				}
			}
		}
	}
}

func (b *broadcaster) Close() {
	for _, n := range b.nodes {
		n.client.Close()
	}
}

// httpRPCToWS converts a subnet-evm HTTP RPC URL into its WebSocket equivalent:
//
//	http://host:port/ext/bc/<id>/rpc  ->  ws://host:port/ext/bc/<id>/ws
//	https://...                       ->  wss://...
func httpRPCToWS(rpcURL string) string {
	u := rpcURL
	switch {
	case strings.HasPrefix(u, "https://"):
		u = "wss://" + strings.TrimPrefix(u, "https://")
	case strings.HasPrefix(u, "http://"):
		u = "ws://" + strings.TrimPrefix(u, "http://")
	}
	if pre, ok := strings.CutSuffix(u, "/rpc"); ok {
		u = pre + "/ws"
	}
	return u
}
