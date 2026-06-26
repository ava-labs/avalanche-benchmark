package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// blockAt fetches the L1 block at the given tag ("finalized", "latest", or a hex
// height like "0x1f4") from a node's RPC, returning (height, hash, ok).
func (c *config) blockAt(i int, tag string) (uint64, string, bool) {
	client := &http.Client{Timeout: 6 * time.Second}
	req := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"eth_getBlockByNumber","params":["%s",false]}`, tag)
	resp, err := client.Post(c.rpcURL(i), "application/json", strings.NewReader(req))
	if err != nil {
		return 0, "", false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var out struct {
		Result *struct {
			Number string `json:"number"`
			Hash   string `json:"hash"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Result == nil {
		return 0, "", false
	}
	h, err := strconv.ParseUint(strings.TrimPrefix(out.Result.Number, "0x"), 16, 64)
	if err != nil {
		return 0, "", false
	}
	return h, out.Result.Hash, true
}

// verifyAgreement proves the post-failover / post-restore invariant: the live
// network is on a SINGLE branch (no fork) and quorum is healthy. It reads each
// uncordoned node's finalized tip, then compares the block HASH at a common
// recent height across all of them — an identical hash on every responding node
// is the proof of a single branch. Validators must all be SERVING. Read-only;
// returns true on PASS. A node merely behind (not yet at the common height) is a
// WARN, not a fork — comparing tips on a live chain is meaningless (sampling
// skew), so the check is deliberately at a fixed common height.
func verifyAgreement(cfg *config) bool {
	topo := cfg.topo
	intents, err := loadIntents(cfg.stateFile, topo)
	if err != nil {
		fatalf("%v", err)
	}
	fmt.Println("== reconcile verify ==")

	type ninfo struct {
		name  string
		idx   int
		h     uint64
		ok    bool
		isVal bool
	}
	var nodes []ninfo
	var maxTip uint64
	for i, in := range intents {
		if in.Cordoned {
			continue
		}
		h, _, ok := cfg.blockAt(i, "finalized")
		nodes = append(nodes, ninfo{topo.MachineName(i), i, h, ok, topo.isValidatorKey(in.Key)})
		if ok && h > maxTip {
			maxTip = h
		}
	}

	want := topo.NVal
	serving := 0
	for _, n := range nodes {
		if n.isVal && n.ok {
			serving++
		}
	}

	// Common height = lowest tip among nodes within tolerance of the max tip (the
	// caught-up set), minus a margin so they all have it. Laggards fall out and are
	// reported separately rather than dragging the comparison to an ancient block.
	common := maxTip
	for _, n := range nodes {
		if n.ok && maxTip-n.h <= syncToleranceBlocks && n.h < common {
			common = n.h
		}
	}
	if common > 16 {
		common -= 16
	}
	tag := fmt.Sprintf("0x%x", common)

	byHash := map[string][]string{}
	var behind []string
	for _, n := range nodes {
		_, hash, ok := cfg.blockAt(n.idx, tag)
		if !ok || hash == "" {
			behind = append(behind, n.name)
			continue
		}
		byHash[hash] = append(byHash[hash], n.name)
	}

	responding := 0
	for _, names := range byHash {
		responding += len(names)
	}

	fmt.Printf("validators serving : %d/%d\n", serving, want)
	fmt.Printf("chain tip (max)    : %d\n", maxTip)
	fmt.Printf("branch check @ block %d:\n", common)
	for hash, names := range byHash {
		short := hash
		if len(short) > 18 {
			short = short[:18]
		}
		fmt.Printf("  %s…  %s\n", short, strings.Join(names, " "))
	}
	if len(behind) > 0 {
		fmt.Printf("  behind (no block %d yet): %s\n", common, strings.Join(behind, " "))
	}

	pass := true
	if serving < want {
		fmt.Printf("FAIL: only %d/%d validators serving (quorum at risk).\n", serving, want)
		pass = false
	}
	switch {
	case len(byHash) > 1:
		fmt.Printf("FAIL: %d distinct hashes at block %d — the network has DIVERGED (fork).\n", len(byHash), common)
		pass = false
	case len(byHash) == 1:
		fmt.Printf("OK: all %d responding nodes share one branch at block %d.\n", responding, common)
	default:
		fmt.Println("FAIL: no node returned the common block — cannot verify.")
		pass = false
	}
	if len(behind) > 0 {
		fmt.Printf("WARN: %d node(s) behind and catching up — re-run verify once they reach tip.\n", len(behind))
	}

	if pass {
		fmt.Println("VERIFY PASS — single branch, quorum healthy.")
	} else {
		fmt.Println("VERIFY FAIL.")
	}
	return pass
}
