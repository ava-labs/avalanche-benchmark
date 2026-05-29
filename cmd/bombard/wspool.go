package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/rpc"
)

// wsPool holds a fixed set of WebSocket-backed JSON-RPC clients spread across
// one or more endpoints. Each client is checked out exclusively for one call at
// a time, so the pool size is a hard concurrency cap and gives natural
// backpressure. Spreading connections across every available RPC means sends
// fan out to all nodes — whichever accepts the tx first wins.
type wsPool struct {
	rpcs    []*rpc.Client
	clients chan *ethclient.Client
}

// newWSPool opens n connections distributed round-robin across the reachable
// wsURLs. Unreachable endpoints are skipped so the run proceeds as long as at
// least one endpoint is alive.
func newWSPool(ctx context.Context, wsURLs []string, n int) (*wsPool, error) {
	if len(wsURLs) == 0 {
		return nil, fmt.Errorf("wsPool needs at least one URL")
	}

	// Probe each endpoint once; keep only the reachable ones.
	var live []string
	for _, url := range wsURLs {
		rc, err := rpc.DialWebsocket(ctx, url, "")
		if err != nil {
			fmt.Printf("WS endpoint unavailable, skipping: %s (%v)\n", url, err)
			continue
		}
		rc.Close()
		live = append(live, url)
	}
	if len(live) == 0 {
		return nil, fmt.Errorf("no reachable WS endpoints among %d", len(wsURLs))
	}
	if n < len(live) {
		n = len(live) // at least one connection per live endpoint
	}

	p := &wsPool{
		rpcs:    make([]*rpc.Client, 0, n),
		clients: make(chan *ethclient.Client, n),
	}
	for i := 0; i < n; i++ {
		url := live[i%len(live)]
		rc, err := rpc.DialWebsocket(ctx, url, "")
		if err != nil {
			fmt.Printf("WS dial failed, skipping connection: %s (%v)\n", url, err)
			continue
		}
		p.rpcs = append(p.rpcs, rc)
		p.clients <- ethclient.NewClient(rc)
	}
	if len(p.rpcs) == 0 {
		p.Close()
		return nil, fmt.Errorf("no WS connections established")
	}
	return p, nil
}

// Do checks out a client, runs fn, returns the client to the pool. Blocks until
// a client is available or ctx is cancelled.
func (p *wsPool) Do(ctx context.Context, fn func(*ethclient.Client) error) error {
	select {
	case c := <-p.clients:
		err := fn(c)
		p.clients <- c
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *wsPool) Close() {
	for _, rc := range p.rpcs {
		rc.Close()
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
