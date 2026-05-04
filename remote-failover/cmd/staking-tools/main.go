// staking-tools is a small helper for the failover lab.
//
// Usage:
//
//	staking-tools gen <out-dir>          Generate signer.key, staker.crt, staker.key
//	staking-tools node-id <staker.crt>   Print NodeID for a TLS staking cert
package main

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/staking"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
)

func main() {
	if len(os.Args) < 3 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "gen":
		if err := genKeys(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "gen failed:", err)
			os.Exit(1)
		}
	case "node-id":
		if err := printNodeID(os.Args[2]); err != nil {
			fmt.Fprintln(os.Stderr, "node-id failed:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  staking-tools gen <out-dir>          Generate signer.key, staker.crt, staker.key")
	fmt.Fprintln(os.Stderr, "  staking-tools node-id <staker.crt>   Print NodeID for a TLS staking cert")
}

func genKeys(outDir string) error {
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	certBytes, keyBytes, err := staking.NewCertAndKeyBytes()
	if err != nil {
		return fmt.Errorf("generate TLS cert: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "staker.crt"), certBytes, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "staker.key"), keyBytes, 0o600); err != nil {
		return err
	}

	signer, err := localsigner.New()
	if err != nil {
		return fmt.Errorf("generate BLS signer: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "signer.key"), signer.ToBytes(), 0o600); err != nil {
		return err
	}
	return nil
}

func printNodeID(certPath string) error {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return fmt.Errorf("no PEM block found in %s", certPath)
	}
	cert, err := staking.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse cert %s: %w", certPath, err)
	}
	fmt.Println(ids.NodeIDFromCert(cert))
	return nil
}
