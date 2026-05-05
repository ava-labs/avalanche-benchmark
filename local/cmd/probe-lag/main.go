// probe-lag: subscribe to newHeads on two endpoints, log per-block arrival
// times, compute DC2 - DC1 lag per block.
package main

import (
	"context"
	"flag"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/ava-labs/libevm/core/types"
	"github.com/ava-labs/libevm/ethclient"
)

func main() {
	dc1WS := flag.String("dc1-ws", "", "DC1 ws URL")
	dc2WS := flag.String("dc2-ws", "", "DC2 ws URL")
	duration := flag.Duration("duration", 30*time.Second, "Sample duration")
	flag.Parse()

	type arrival struct {
		num  uint64
		t    time.Time
	}
	dc1Arrivals := map[uint64]time.Time{}
	dc2Arrivals := map[uint64]time.Time{}
	var mu sync.Mutex

	ctx, cancel := context.WithTimeout(context.Background(), *duration)
	defer cancel()

	subscribe := func(label, url string, store map[uint64]time.Time) {
		c, err := ethclient.Dial(url)
		if err != nil {
			fmt.Printf("[%s] dial: %v\n", label, err)
			return
		}
		defer c.Close()
		ch := make(chan *types.Header, 1024)
		sub, err := c.SubscribeNewHead(ctx, ch)
		if err != nil {
			fmt.Printf("[%s] subscribe: %v\n", label, err)
			return
		}
		defer sub.Unsubscribe()
		fmt.Printf("[%s] subscribed to %s\n", label, url)
		for {
			select {
			case <-ctx.Done():
				return
			case h := <-ch:
				now := time.Now()
				mu.Lock()
				if _, exists := store[h.Number.Uint64()]; !exists {
					store[h.Number.Uint64()] = now
				}
				mu.Unlock()
			case err := <-sub.Err():
				fmt.Printf("[%s] sub err: %v\n", label, err)
				return
			}
		}
	}

	go subscribe("DC1", *dc1WS, dc1Arrivals)
	go subscribe("DC2", *dc2WS, dc2Arrivals)

	<-ctx.Done()
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	type pair struct {
		num    uint64
		lagMs  float64
	}
	var pairs []pair
	for n, t1 := range dc1Arrivals {
		if t2, ok := dc2Arrivals[n]; ok {
			pairs = append(pairs, pair{num: n, lagMs: float64(t2.Sub(t1).Microseconds()) / 1000})
		}
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].num < pairs[j].num })

	fmt.Printf("DC1 events=%d DC2 events=%d overlap=%d\n", len(dc1Arrivals), len(dc2Arrivals), len(pairs))
	if len(dc1Arrivals) > 0 {
		var mn, mx uint64 = ^uint64(0), 0
		for n := range dc1Arrivals {
			if n < mn { mn = n }
			if n > mx { mx = n }
		}
		fmt.Printf("DC1 block range: %d..%d\n", mn, mx)
	}
	if len(dc2Arrivals) > 0 {
		var mn, mx uint64 = ^uint64(0), 0
		for n := range dc2Arrivals {
			if n < mn { mn = n }
			if n > mx { mx = n }
		}
		fmt.Printf("DC2 block range: %d..%d\n", mn, mx)
	}
	if len(pairs) == 0 {
		fmt.Println("no overlap")
		return
	}

	lags := make([]float64, len(pairs))
	var sum, mn, mx float64
	mn = pairs[0].lagMs
	mx = pairs[0].lagMs
	for i, p := range pairs {
		lags[i] = p.lagMs
		sum += p.lagMs
		if p.lagMs < mn {
			mn = p.lagMs
		}
		if p.lagMs > mx {
			mx = p.lagMs
		}
	}
	sort.Float64s(lags)
	p50 := lags[len(lags)/2]
	p95 := lags[len(lags)*95/100]

	fmt.Printf("\nDC2-fire MINUS DC1-fire (as observed from this host):\n")
	fmt.Printf("  blocks=%d mean=%.1fms p50=%.1fms p95=%.1fms min=%.1fms max=%.1fms\n",
		len(lags), sum/float64(len(lags)), p50, p95, mn, mx)
	fmt.Printf("  (note: observation has WAN bias if run from one DC; subtract host->DC1 RTT one-way to estimate true lag)\n")
}
