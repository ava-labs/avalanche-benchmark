package main

import (
	"fmt"
	"os"
	"strconv"
)

// instance describes ONE avalanchego process in the pool. In the validated prod
// layout every pool machine runs exactly one — 6 distinct IPs per site, one process
// each. To let a smaller machine count host the full fixed topology (e.g. a 3-box
// test running all 6 slots of a site), an IP may REPEAT in NODE_IPS /
// BACKUP_SITE_NODE_IPS; each repeat launches another avalanchego on the same box,
// offset onto its own ports and data/staking subdirs so the co-located processes
// never collide. The 6-slots-per-site topology is unchanged — only WHERE each slot
// physically runs becomes flexible.
//
// The FIRST occurrence of a host (idx 0) uses the exact ports and paths the
// single-process deploy has always used (9652/9653, data/validator, staking/active,
// chain-config.json). So a deployment of 6 distinct IPs is byte-identical to the
// proven path; only a co-located 2nd+ instance on a box gets the "-N" dir suffix and
// the +10*N port bump.
//
// NOTE: co-location is a TEST affordance. It trades away fault isolation — a box
// hosting two nodes loses both at once — so it is NOT a representative DR test (the
// failover/restore guarantees assume the redundancy pairs live on separate boxes).
// loadConfig warns when a box carries more than one node.
type instance struct {
	host        string // physical node IP
	idx         int    // 0-based occurrence of host within the pool (0 = first/only on that box)
	shared      bool   // host runs more than one instance (a co-located test box)
	httpPort    int    // 9652 + 10*idx
	stakingPort int    // 9653 + 10*idx
	dataDir     string // relative to remoteDir: "data/validator"      | "data/validator-N"
	activeDir   string // relative to remoteDir: "staking/active"      | "staking/active-N"
	chainCfg    string // role chain-config filename:  "chain-config.json"     | "chain-config-N.json"
	startScript string // launch script filename:      "start-l1-validator.sh" | "start-l1-validator-N.sh"
	procPat     string // pgrep/pkill -f pattern matching ONLY this process (bracketed to avoid self-match)
}

const (
	baseHTTPPort          = 9652
	baseStakingPort       = 9653
	portStridePerInstance = 10 // a co-located instance's ports are bumped by this * its occurrence index
)

// makeInstance derives the ports and on-disk paths for the idx-th avalanchego
// process on host. idx 0 reproduces the single-process layout exactly.
func makeInstance(host string, idx int) instance {
	suffix := ""
	if idx > 0 {
		suffix = "-" + strconv.Itoa(idx)
	}
	http := baseHTTPPort + portStridePerInstance*idx
	portStr := strconv.Itoa(http)
	// Bracket the first digit so the literal pgrep/pkill argv ("--http-port=[9]652")
	// is NOT matched by its own regex ("--http-port=9652") — same self-match guard the
	// plugin reaper uses with pluginPat. Only needed when a box is shared (see killNode).
	procPat := "--http-port=[" + portStr[:1] + "]" + portStr[1:]
	return instance{
		host:        host,
		idx:         idx,
		httpPort:    http,
		stakingPort: baseStakingPort + portStridePerInstance*idx,
		dataDir:     "data/validator" + suffix,
		activeDir:   "staking/active" + suffix,
		chainCfg:    "chain-config" + suffix + ".json",
		startScript: "start-l1-validator" + suffix + ".sh",
		procPat:     procPat,
	}
}

// buildInstances assigns an instance to every pool slot. A repeated IP's occurrence
// index is its order of appearance in the pool, so co-located processes get stable,
// deterministic ports/dirs no matter which site slot they land in.
func buildInstances(pool []string) []instance {
	total := map[string]int{}
	for _, h := range pool {
		total[h]++
	}
	occ := map[string]int{}
	insts := make([]instance, len(pool))
	for i, h := range pool {
		insts[i] = makeInstance(h, occ[h])
		insts[i].shared = total[h] > 1
		occ[h]++
	}
	return insts
}

// instancesOnHost returns the pool indexes whose physical host is host, ascending.
// Used to drive shared per-host work (upload, provisioning) once per box rather than
// once per co-located process.
func (c *config) instancesOnHost(host string) []int {
	var idx []int
	for i := range c.instances {
		if c.instances[i].host == host {
			idx = append(idx, i)
		}
	}
	return idx
}

// slotRoleName labels a pool slot by its role within its site
// ([v1..vN, spare..., rpc...]). Human-facing only — endpoints + co-location warnings.
func (t Topology) slotRoleName(i int) string {
	switch s := t.slotInSite(i); {
	case s < t.NVal:
		return "v" + strconv.Itoa(s+1)
	case s < t.NVal+t.NSpare:
		return "spare"
	default:
		return "rpc"
	}
}

// warnColocation prints a CONCISE heads-up (to stderr) when the topology trades away
// fault isolation: co-located nodes, validators sharing a box, or a single RPC. Kept
// to a couple of lines and stderr-only so it never buries command output; callers
// skip it entirely for the read-only `status`/`verify` (often run under `watch`).
func (c *config) warnColocation() {
	if c.topo.NRPC < 2 {
		fmt.Fprintln(os.Stderr, "note: 1 RPC/site — a restore briefly stops your only ingress node (2+ recommended).")
	}
	hosts, valShareHosts := 0, 0
	seen := map[string]bool{}
	for _, in := range c.instances {
		if !in.shared || seen[in.host] {
			continue
		}
		seen[in.host] = true
		hosts++
		vals := 0
		for _, i := range c.instancesOnHost(in.host) {
			if c.topo.isValidatorSlot(i) {
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
	fmt.Fprintf(os.Stderr, "note: co-location TEST mode — %d host(s) run >1 node; a box loss takes all co-located nodes (not a representative DR test).\n", hosts)
	if valShareHosts > 0 {
		fmt.Fprintf(os.Stderr, "      %d host(s) carry 2+ validators → a single box loss could drop quorum.\n", valShareHosts)
	}
}
