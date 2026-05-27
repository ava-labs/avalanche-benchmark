package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/ava-labs/libevm/ethclient"
	"github.com/ava-labs/libevm/rpc"
)

// wsPool holds a fixed set of WebSocket-backed JSON-RPC clients. Each client
// is checked out exclusively for one call at a time, so the pool size acts as
// a hard concurrency cap — when all connections are busy, callers block until
// a response comes back. This gives us natural backpressure: if the node slows
// down, fewer requests get through.
type wsPool struct {
	rpcs    []*rpc.Client
	clients chan *ethclient.Client
}

func newWSPool(ctx context.Context, wsURL string, n int) (*wsPool, error) {
	if n <= 0 {
		return nil, fmt.Errorf("wsPool size must be > 0, got %d", n)
	}
	p := &wsPool{
		rpcs:    make([]*rpc.Client, 0, n),
		clients: make(chan *ethclient.Client, n),
	}
	for i := range n {
		rc, err := rpc.DialWebsocket(ctx, wsURL, "")
		if err != nil {
			p.Close()
			return nil, fmt.Errorf("dial ws conn %d: %w", i, err)
		}
		p.rpcs = append(p.rpcs, rc)
		p.clients <- ethclient.NewClient(rc)
	}
	return p, nil
}

// Do checks out a client, runs fn, returns the client to the pool. Blocks
// until a client is available or ctx is cancelled.
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
