// Command genstaking generates the per-deploy staking identities for every
// node in nodes.ini that does not have one yet: staker.crt / staker.key under
// staking/l1/<name>/, plus the BLS signer.key for role=validator nodes only
// (rpc nodes are never registered, so no signer key ever exists or ships for
// them). It then (re)writes the staking/node-ids.env manifest
// (<name>=<NodeID> lines). The identities are GITIGNORED and never committed:
// their NodeIDs get bound as validationIDs on a public P-chain, so a leaked
// staking key equals validator impersonation. Invoked by
// ./setup/00_gen_secrets.sh (as bin/genstaking); `l1 create` generates any
// missing VALIDATOR identity on its own through the same internal/vset path.
//
// Usage: genstaking (no arguments; run from the repo root)
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/topo"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/vset"
)

const stakingDir = "staking"

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genstaking: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) != 1 {
		fatalf("usage: genstaking (no arguments; nodes come from nodes.ini)")
	}
	nodes, err := topo.LoadNear()
	if err != nil {
		fatalf("%v", err)
	}
	if err := vset.CheckNamedKeyDirs(stakingDir); err != nil {
		fatalf("%v", err)
	}
	existing := map[string]vset.Entry{}
	if entries, err := vset.ReadManifest(stakingDir); err == nil {
		for _, e := range entries {
			existing[e.Name] = e
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		fatalf("%v", err) // a present-but-broken manifest (e.g. numbered format) must stop us
	}

	generated := 0
	for _, n := range nodes {
		dir := filepath.Join(stakingDir, "l1", n.Name)
		if _, err := os.Stat(dir); err == nil {
			id, err := vset.NodeIDFromCertFile(filepath.Join(dir, "staker.crt"))
			if err != nil {
				fatalf("%v", err)
			}
			if e, ok := existing[n.Name]; ok && e.NodeID != id {
				fatalf("%s staker.crt yields %s but node-ids.env says %s: manifest and keys disagree", dir, id, e.NodeID)
			}
			existing[n.Name] = vset.Entry{Name: n.Name, NodeID: id}
			fmt.Printf("  %s exists (%s): keeping it\n", dir, id)
			continue
		}
		id, err := vset.GenerateIdentity(stakingDir, n.Name, n.IsValidator())
		if err != nil {
			fatalf("%v", err)
		}
		existing[n.Name] = vset.Entry{Name: n.Name, NodeID: id}
		fmt.Printf("  generated %s (%s, role=%s)\n", dir, id, n.Role)
		generated++
	}

	entries := make([]vset.Entry, 0, len(existing))
	for _, e := range existing {
		entries = append(entries, e)
	}
	if err := vset.WriteManifest(stakingDir, entries); err != nil {
		fatalf("write node-ids.env: %v", err)
	}
	fmt.Printf("%d identity(ies) generated, manifest %s/node-ids.env updated (%d entries)\n",
		generated, stakingDir, len(entries))
}
