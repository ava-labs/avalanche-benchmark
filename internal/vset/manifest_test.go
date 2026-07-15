package vset

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManifestRoundTripAndIdentity(t *testing.T) {
	dir := t.TempDir()

	id1, err := GenerateIdentity(dir, "l1", "a1", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateIdentity(dir, "l1", "a1", true); err == nil {
		t.Fatal("GenerateIdentity must refuse to overwrite")
	}
	idRPC, err := GenerateIdentity(dir, "l1", "rpc_a1", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"staker.crt", "staker.key", "signer.key"} {
		if _, err := os.Stat(filepath.Join(dir, "l1", "a1", f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	// rpc identities never get a BLS signer key.
	if _, err := os.Stat(filepath.Join(dir, "l1", "rpc_a1", "signer.key")); err == nil {
		t.Error("rpc identity must not have a signer.key")
	}
	if got, err := NodeIDFromCertFile(filepath.Join(dir, "l1", "a1", "staker.crt")); err != nil || got != id1 {
		t.Errorf("NodeIDFromCertFile = %s (%v), want %s", got, err, id1)
	}

	in := []Entry{
		{Name: "rpc_a1", NodeID: idRPC},
		{Name: "a1", NodeID: id1},
	}
	if err := WriteManifest(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 || out[0].Name != "a1" || out[0].NodeID != id1 ||
		out[1].Name != "rpc_a1" || out[1].NodeID != idRPC {
		t.Fatalf("ReadManifest = %+v", out)
	}

	if err := CheckNamedKeyDirs(dir); err != nil {
		t.Errorf("named layout must pass: %v", err)
	}
}

func TestNumberedLayoutRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "node-ids.env"),
		[]byte("L1_1_NODE_ID=NodeID-K2UkKZfq5asStFMBSmWFEjMSVKfDGgzbR\nL1_1_NAME=a1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadManifest(dir); err == nil || !strings.Contains(err.Error(), "numbered format") {
		t.Errorf("old manifest: err = %v, want numbered-format rejection", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "l1", "3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckNamedKeyDirs(dir); err == nil || !strings.Contains(err.Error(), "numbered key dir") {
		t.Errorf("numbered dir: err = %v, want numbered-key-dir rejection", err)
	}
}
