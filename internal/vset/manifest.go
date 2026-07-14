package vset

import (
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/staking"
	"github.com/ava-labs/avalanchego/utils/crypto/bls/signer/localsigner"
	"github.com/joho/godotenv"
)

// Entry is one committed staking identity from the staking/node-ids.env
// manifest. Key is the staking/l1/<key> directory index; Name is the
// operator-facing validator name (a1../b1.., written by `l1 create`; empty
// for RPC identities and manifests predating names).
type Entry struct {
	Key    int
	NodeID ids.NodeID
	Name   string
}

func manifestPath(stakingDir string) string {
	return filepath.Join(stakingDir, "node-ids.env")
}

// ReadManifest parses staking/node-ids.env (L1_<k>_NODE_ID plus optional
// L1_<k>_NAME lines) into entries sorted by key.
func ReadManifest(stakingDir string) ([]Entry, error) {
	p := manifestPath(stakingDir)
	vars, err := godotenv.Read(p)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	byKey := map[int]*Entry{}
	entry := func(k int) *Entry {
		if byKey[k] == nil {
			byKey[k] = &Entry{Key: k}
		}
		return byKey[k]
	}
	for k, v := range vars {
		var key int
		if _, err := fmt.Sscanf(k, "L1_%d_NODE_ID", &key); err == nil && strings.HasSuffix(k, "_NODE_ID") {
			id, err := ids.NodeIDFromString(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("parse %s in %s: %w", k, p, err)
			}
			entry(key).NodeID = id
			continue
		}
		if _, err := fmt.Sscanf(k, "L1_%d_NAME", &key); err == nil && strings.HasSuffix(k, "_NAME") {
			entry(key).Name = strings.TrimSpace(v)
		}
	}
	out := make([]Entry, 0, len(byKey))
	for _, e := range byKey {
		if e.NodeID == ids.EmptyNodeID {
			return nil, fmt.Errorf("%s: L1_%d_NAME has no matching L1_%d_NODE_ID", p, e.Key, e.Key)
		}
		out = append(out, *e)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Key < out[b].Key })
	return out, nil
}

// WriteManifest renders the entries back to staking/node-ids.env, sorted by
// key, one L1_<k>_NODE_ID line each plus an L1_<k>_NAME line when named.
func WriteManifest(stakingDir string, entries []Entry) error {
	sorted := append([]Entry(nil), entries...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a].Key < sorted[b].Key })
	var b strings.Builder
	for _, e := range sorted {
		fmt.Fprintf(&b, "L1_%d_NODE_ID=%s\n", e.Key, e.NodeID)
		if e.Name != "" {
			fmt.Fprintf(&b, "L1_%d_NAME=%s\n", e.Key, e.Name)
		}
	}
	return os.WriteFile(manifestPath(stakingDir), []byte(b.String()), 0o644)
}

// GenerateIdentity creates the full node identity for staking/l1/<key>
// (staker.crt + staker.key TLS identity that IS the NodeID, plus the BLS
// signer.key) and returns the NodeID. It refuses to overwrite: an existing
// identity may be registered on a public P-chain.
func GenerateIdentity(stakingDir string, key int) (ids.NodeID, error) {
	dir := filepath.Join(stakingDir, "l1", strconv.Itoa(key))
	if _, err := os.Stat(dir); err == nil {
		return ids.EmptyNodeID, fmt.Errorf("%s already exists: refusing to overwrite an identity that may be registered on-chain", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ids.EmptyNodeID, fmt.Errorf("mkdir %s: %w", dir, err)
	}
	certPEM, keyPEM, err := staking.NewCertAndKeyBytes()
	if err != nil {
		return ids.EmptyNodeID, fmt.Errorf("generate TLS identity %d: %w", key, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "staker.crt"), certPEM, 0o644); err != nil {
		return ids.EmptyNodeID, err
	}
	if err := os.WriteFile(filepath.Join(dir, "staker.key"), keyPEM, 0o600); err != nil {
		return ids.EmptyNodeID, err
	}
	signer, err := localsigner.New()
	if err != nil {
		return ids.EmptyNodeID, fmt.Errorf("generate BLS key %d: %w", key, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "signer.key"), signer.ToBytes(), 0o600); err != nil {
		return ids.EmptyNodeID, err
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
