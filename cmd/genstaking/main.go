// Command genstaking generates per-deploy staking identities
// (staker.crt / staker.key / signer.key) under staking/l1/<index>/ for the
// given key indices and prints the matching node-ids.env manifest lines.
// The identities are GITIGNORED and never committed: their NodeIDs get bound
// as validationIDs on a public P-chain, so a leaked staking key equals
// validator impersonation. Invoked by ./setup/00_gen_secrets.sh (as
// bin/genstaking); `l1 create` generates any missing VALIDATOR identities on
// its own through the same internal/vset code path, this tool also covers the
// RPC tier's identities.
//
// Usage: genstaking <firstIndex> <lastIndex>
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/vset"
)

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "genstaking: "+format+"\n", args...)
	os.Exit(1)
}

func main() {
	if len(os.Args) != 3 {
		fatalf("usage: genstaking <firstIndex> <lastIndex>")
	}
	lo, err1 := strconv.Atoi(os.Args[1])
	hi, err2 := strconv.Atoi(os.Args[2])
	if err1 != nil || err2 != nil || lo < 1 || hi < lo {
		fatalf("bad index range %q..%q", os.Args[1], os.Args[2])
	}
	for idx := lo; idx <= hi; idx++ {
		id, err := vset.GenerateIdentity("staking", idx)
		if err != nil {
			fatalf("%v", err)
		}
		fmt.Printf("L1_%d_NODE_ID=%s\n", idx, id)
	}
}
