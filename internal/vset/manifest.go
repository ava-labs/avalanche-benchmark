package vset

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/staking"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/joho/godotenv"
)

// Entry is one committed staking identity from the staking/node-ids.env
// manifest: the node name (the nodes.ini primary key, also the
// staking/l1/<name> directory) and its NodeID.
type Entry struct {
	Name   string
	NodeID ids.NodeID
}

func manifestPath(stakingDir string) string {
	return filepath.Join(stakingDir, "node-ids.env")
}

// migrateHint points an operator holding the retired numbered layout at the
// rename procedure.
const migrateHint = `rename each staking/l1/<N> dir to its node name and rewrite node-ids.env as <name>=<NodeID> lines (see README "Migrating from numbered key dirs")`

// ReadManifest parses staking/node-ids.env (<name>=<NodeID> lines) into
// entries sorted by name. A manifest in the retired numbered format
// (L1_<n>_NODE_ID) is an error pointing at the migration procedure.
func ReadManifest(stakingDir string) ([]Entry, error) {
	p := manifestPath(stakingDir)
	vars, err := godotenv.Read(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	out := make([]Entry, 0, len(vars))
	for k, v := range vars {
		if numberedManifestKey(k) {
			return nil, fmt.Errorf("%s still uses the retired numbered format (%s=...): %s", p, k, migrateHint)
		}
		id, err := ids.NodeIDFromString(strings.TrimSpace(v))
		if err != nil {
			return nil, fmt.Errorf("parse %s in %s: %w", k, p, err)
		}
		out = append(out, Entry{Name: k, NodeID: id})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// numberedManifestKey matches the retired L1_<n>_NODE_ID / L1_<n>_NAME keys.
func numberedManifestKey(k string) bool {
	rest, ok := strings.CutPrefix(k, "L1_")
	if !ok {
		return false
	}
	digits, ok := strings.CutSuffix(rest, "_NODE_ID")
	if !ok {
		digits, ok = strings.CutSuffix(rest, "_NAME")
	}
	if !ok || digits == "" {
		return false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// WriteManifest renders the entries back to staking/node-ids.env, one
// <name>=<NodeID> line each, sorted by name.
func WriteManifest(stakingDir string, entries []Entry) error {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].Name < sorted[b].Name })
	var b strings.Builder
	for _, e := range sorted {
		fmt.Fprintf(&b, "%s=%s\n", e.Name, e.NodeID)
	}
	return os.WriteFile(manifestPath(stakingDir), []byte(b.String()), 0o644)
}

// CheckNamedKeyDirs errors when staking/l1 still holds a numbered key dir
// from the retired layout (staking/l1/3 instead of staking/l1/a3). A missing
// staking/l1 is fine: nothing generated yet.
func CheckNamedKeyDirs(stakingDir string) error {
	entries, err := os.ReadDir(filepath.Join(stakingDir, "l1"))
	if err != nil {
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		allDigits := name != ""
		for _, r := range name {
			if r < '0' || r > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			return fmt.Errorf("%s is a numbered key dir from the retired layout: %s",
				filepath.Join(stakingDir, "l1", name), migrateHint)
		}
	}
	return nil
}

// GenerateIdentity creates the node identity for staking/<tier>/<name>: the
// staker.crt + staker.key TLS identity that IS the NodeID, plus - for
// validators only - the BLS signer.key (withSigner; rpc nodes are never
// registered so no signer key ever exists for them). tier is "l1" for the
// fleet nodes and "manager" for the phantom manager-L1 signing committee. It
// refuses to overwrite: an existing identity may be registered on a public
// P-chain.
func GenerateIdentity(stakingDir, tier, name string, withSigner bool) (ids.NodeID, error) {
	dir := filepath.Join(stakingDir, tier, name)
	if _, err := os.Stat(dir); err == nil {
		return ids.EmptyNodeID, fmt.Errorf("%s already exists: refusing to overwrite an identity that may be registered on-chain", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ids.EmptyNodeID, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	certPEM, keyPEM, err := staking.NewCertAndKeyBytes()
	if err != nil {
		return ids.EmptyNodeID, fmt.Errorf("generate TLS identity %s: %w", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staker.crt"), certPEM, 0o644); err != nil {
		return ids.EmptyNodeID, err
	}
	if err := os.WriteFile(filepath.Join(dir, "staker.key"), keyPEM, 0o600); err != nil {
		return ids.EmptyNodeID, err
	}
	if withSigner {
		signer, err := localsigner.New()
		if err != nil {
			return ids.EmptyNodeID, fmt.Errorf("generate BLS key %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "signer.key"), signer.ToBytes(), 0o600); err != nil {
			return ids.EmptyNodeID, err
		}
	}
	return NodeIDFromCertFile(filepath.Join(dir, "staker.crt"))
}

// NodeIDFromCertFile derives the NodeID from a staker.crt.
func NodeIDFromCertFile(certPath string) (ids.NodeID, error) {
	pemBytes, err := os.ReadFile(certPath)
	if err != nil {
		return ids.EmptyNodeID, err
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return ids.EmptyNodeID, fmt.Errorf("no PEM block in %s", certPath)
	}
	cert, err := staking.ParseCertificate(block.Bytes)
	if err != nil {
		return ids.EmptyNodeID, fmt.Errorf("parse %s: %w", certPath, err)
	}
	return ids.NodeIDFromCert(cert), nil
}
