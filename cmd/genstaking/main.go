// Command genstaking generates committed benchmark staking identities
// (staker.crt / staker.key / signer.key) under staking/l1/<index>/ for the
// given key indices and prints the matching node-ids.env lines. These are
// throwaway test identities for the failover benchmark — committing them is
// intentional (see staking/node-ids.env).
//
// Usage: go run ./cmd/genstaking <firstIndex> <lastIndex>
package main

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/staking"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
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
		dir := filepath.Join("staking", "l1", strconv.Itoa(idx))
		if _, err := os.Stat(dir); err == nil {
			fatalf("%s already exists — refusing to overwrite a committed identity", dir)
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatalf("mkdir %s: %v", dir, err)
		}

		certPEM, keyPEM, err := staking.NewCertAndKeyBytes()
		if err != nil {
			fatalf("generate TLS identity %d: %v", idx, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "staker.crt"), certPEM, 0o644); err != nil {
			fatalf("write staker.crt: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "staker.key"), keyPEM, 0o600); err != nil {
			fatalf("write staker.key: %v", err)
		}

		signer, err := localsigner.New()
		if err != nil {
			fatalf("generate BLS key %d: %v", idx, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "signer.key"), signer.ToBytes(), 0o600); err != nil {
			fatalf("write signer.key: %v", err)
		}

		block, _ := pem.Decode(certPEM)
		if block == nil {
			fatalf("identity %d: cert is not PEM", idx)
		}
		cert, err := staking.ParseCertificate(block.Bytes)
		if err != nil {
			fatalf("identity %d: parse cert: %v", idx, err)
		}
		fmt.Printf("L1_%d_NODE_ID=%s\n", idx, ids.NodeIDFromCert(cert))
	}
}
