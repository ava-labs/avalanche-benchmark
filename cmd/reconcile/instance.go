package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
)

// instance describes ONE avalanchego process: the on-box paths and process
// pattern derived from its nodes.ini entry. Every node owns one data root on
// its box, data/<name>, holding its db, chainData, logs and the active
// staking identity, so nodes sharing a host never collide. Ports come from
// nodes.ini's positional per-host assignment (the first node on a host is
// 9650/9651, the next 9652/9653, ...).
//
// NOTE: hosting more than one node on a box trades away fault isolation - a
// box loss takes every node on it - so it is a TEST affordance, not a
// representative DR shape. loadConfig warns when a box carries several nodes.
type instance struct {
	host        string // physical node IP
	shared      bool   // host runs more than one node (a co-hosted test box)
	httpPort    int
	stakingPort int
	dataDir     string // node root, relative to remoteDir: "data/<name>"
	activeDir   string // active staking identity: "data/<name>/staking/active"
	startScript string // launch script filename: "start-<name>.sh"
	procPat     string // pgrep/pkill -f pattern matching ONLY this process (bracketed to avoid self-match)
}

// makeInstance derives the ports and on-disk paths for a node. The bracketed
// first port digit makes the literal pgrep/pkill argv ("--http-port=[9]652")
// not match its own regex ("--http-port=9652") - the same self-match guard
// the plugin reaper uses with pluginPat.
func makeInstance(n topo.Node) instance {
	portStr := strconv.Itoa(n.Port)
	root := "data/" + n.Name
	return instance{
		host:        n.Host,
		httpPort:    n.Port,
		stakingPort: n.StakingPort(),
		dataDir:     root,
		activeDir:   root + "/staking/active",
		startScript: "start-" + n.Name + ".sh",
		procPat:     "--http-port=[" + portStr[:1] + "]" + portStr[1:],
	}
}

// buildInstances assigns an instance to every node, parallel to nodes.
func buildInstances(nodes []topo.Node) []instance {
	total := map[string]int{}
	for _, n := range nodes {
		total[n.Host]++
	}
	insts := make([]instance, len(nodes))
	for i, n := range nodes {
		insts[i] = makeInstance(n)
		insts[i].shared = total[n.Host] > 1
	}
	return insts
}

// instancesOnHost returns the node indexes whose physical host is host, in
// nodes.ini order. Used to drive shared per-host work (upload, provisioning)
// once per box rather than once per co-hosted node.
func (c *config) instancesOnHost(host string) []int {
	var idx []int
	for i := range c.instances {
		if c.instances[i].host == host {
			idx = append(idx, i)
		}
	}
	return idx
}

// warnColocation prints a CONCISE heads-up (to stderr) when the inventory
// trades away fault isolation: co-hosted nodes or validators sharing a box.
// Kept to a couple of lines and stderr-only so it never buries command
// output; the read-only `status` (often run under `watch`) skips it entirely.
func (c *config) warnColocation() {
	hosts, valShareHosts := 0, 0
	seen := map[string]bool{}
	for i := range c.instances {
		in := c.instances[i]
		if !in.shared || seen[in.host] {
			continue
		}
		seen[in.host] = true
		hosts++
		vals := 0
		for _, j := range c.instancesOnHost(in.host) {
			if c.nodes[j].IsValidator() {
				vals++
			}
		}
		if vals > 1 {
			valShareHosts++
		}
	}
	if hosts == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "note: co-hosting TEST mode - %d host(s) run >1 node; a box loss takes all its nodes (not a representative DR test).\n", hosts)
	if valShareHosts > 0 {
		fmt.Fprintf(os.Stderr, "      %d host(s) carry 2+ validators → a single box loss could drop quorum.\n", valShareHosts)
	}
}
