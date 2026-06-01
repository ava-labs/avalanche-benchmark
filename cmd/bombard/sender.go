package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
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
	url    string
	client *ethclient.Client
	queue  chan *types.Transaction
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
		b.nodes = append(b.nodes, ns)
		for i := 0; i < sendConcPerNode; i++ {
			go ns.run(ctx, sendTimeout)
		}
	}
	if len(b.nodes) == 0 {
		return nil, fmt.Errorf("no usable endpoints among %d", len(rpcURLs))
	}
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
		select {
		case n.queue <- signed:
		default:
			// Node is saturated/down; drop. Other nodes still get it and the
			// resubmit loop will retry.
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
