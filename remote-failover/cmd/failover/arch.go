package main

import (
	"fmt"
	"strconv"
	"strings"
)

// archSpec describes how the 5 validator NodeIDs (registered on P-chain
// at L1 creation) are placed across the 5 DC1 + 5 DC2 chain hosts.
// dc1+dc2 must equal 5 -- the validator set size is fixed by createL1.
//
// Followers (hosts not assigned a validator key) boot with auto-generated
// staking keys; they track the subnet but their NodeIDs are not registered.
type archSpec struct {
	dc1, dc2 int
}

func (a archSpec) String() string {
	return fmt.Sprintf("%d+%d", a.dc1, a.dc2)
}

// parseArch accepts strings like "5+0", "4+1", "3+2".
func parseArch(s string) (archSpec, error) {
	parts := strings.SplitN(s, "+", 2)
	if len(parts) != 2 {
		return archSpec{}, fmt.Errorf("arch %q must be of form N+M (e.g. 3+2)", s)
	}
	a, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		return archSpec{}, fmt.Errorf("arch %q: parse DC1 count: %w", s, err)
	}
	b, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		return archSpec{}, fmt.Errorf("arch %q: parse DC2 count: %w", s, err)
	}
	if a < 0 || b < 0 {
		return archSpec{}, fmt.Errorf("arch %q: counts must be non-negative", s)
	}
	if a+b != 5 {
		return archSpec{}, fmt.Errorf("arch %q: validator set size is 5; got %d+%d=%d", s, a, b, a+b)
	}
	if a > 5 || b > 5 {
		return archSpec{}, fmt.Errorf("arch %q: each DC has at most 5 hosts", s)
	}
	return archSpec{dc1: a, dc2: b}, nil
}

// keyAssignments returns a map from chain-node IP to the staker-key index
// (1..5, matching staking/dc1/<idx>/) the host should boot with. Hosts
// not in the map boot keyless and act as followers.
func keyAssignments(cfg *config) map[string]int {
	a := make(map[string]int, 5)
	keyIdx := 1
	for i := 0; i < cfg.arch.dc1; i++ {
		a[cfg.dc1IPs[i]] = keyIdx
		keyIdx++
	}
	for i := 0; i < cfg.arch.dc2; i++ {
		a[cfg.dc2IPs[i]] = keyIdx
		keyIdx++
	}
	return a
}

// roleLabel returns a short tag for log lines, e.g. "dc1-v1" for a DC1
// validator holding key index 1, or "dc2-fol" for a DC2 follower.
func roleLabel(cfg *config, host string, keyIdx int, isValidator bool) string {
	dc := "dc2"
	for _, ip := range cfg.dc1IPs {
		if ip == host {
			dc = "dc1"
			break
		}
	}
	if isValidator {
		return fmt.Sprintf("%s-v%d", dc, keyIdx)
	}
	return dc + "-fol"
}
