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
	// fills its queue and then drops — it never blocks the issuer or other
	// nodes. Resubmission and the other nodes cover the dropped sends.
	sendQueueLen = 4096

	// Healthy ingress routing (mirrors a production load-balancer health check):
	// drop an endpoint from the send rotation once it falls ingressDropBehind
	// blocks behind the furthest-ahead endpoint, and re-add it once within
	// ingressRejoinWithin. Routing ingress to a node that isn't caught up wastes
	// sends and — worse — keeps a recovering node (e.g. a wiped RPC) from ever
	// catching up. Hysteresis (drop >> rejoin) avoids flapping at the boundary.
	ingressDropBehind    = 200
	ingressRejoinWithin  = 50
	ingressCheckInterval = 3 * time.Second
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
	healthy atomic.Bool // routed ingress only while caught up to tip
}

// newBroadcaster dials every rpcURL (lazily, over HTTP — a down node is included
// and simply errors on send) behind a single tuned transport and starts the
// per-node sender pools. sendTimeout bounds each individual send.
func newBroadcaster(ctx context.Context, rpcURLs []string, sendTimeout time.Duration) (*broadcaster, error) {
	if len(rpcURLs) == 0 {
		return nil, fmt.Errorf("broadcaster needs at least one URL")
	}

	// One shared transport. Keep-alive connections are pooled per host, bounded
	// at sendConcPerNode so we never exceed what the sender goroutines can use —
	// no ephemeral-port churn, no fd blowup.
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
// ignoring all errors (already-known, nonce races, a down node — all expected).
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
	for _, n := range b.nodes {
		if !n.healthy.Load() {
			continue // behind tip / not caught up — out of ingress rotation
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
// caught up to the tip — mirroring a load-balancer health check. A node that has
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
				continue // nothing reachable yet — leave routing unchanged
			}
			for i, n := range b.nodes {
				behind := maxH - heights[i]
				switch {
				case n.healthy.Load() && behind > ingressDropBehind:
					n.healthy.Store(false)
					fmt.Fprintf(os.Stderr, "ingress: %s out of rotation — %d blocks behind tip (catching up)\n", n.url, behind)
				case !n.healthy.Load() && behind <= ingressRejoinWithin:
					n.healthy.Store(true)
					fmt.Fprintf(os.Stderr, "ingress: %s back in rotation — caught up (%d behind)\n", n.url, behind)
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
