package vset

import (
	"os"
	"path/filepath"
	"testing"
)

func TestManifestRoundTripAndIdentity(t *testing.T) {
	dir := t.TempDir()

	id1, err := GenerateIdentity(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := GenerateIdentity(dir, 1); err == nil {
		t.Fatal("GenerateIdentity must refuse to overwrite")
	}
	id2, err := GenerateIdentity(dir, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"staker.crt", "staker.key", "signer.key"} {
		if _, err := os.Stat(filepath.Join(dir, "l1", "1", f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if got, err := NodeIDFromCertFile(filepath.Join(dir, "l1", "1", "staker.crt")); err != nil || got != id1 {
		t.Errorf("NodeIDFromCertFile = %s (%v), want %s", got, err, id1)
	}

	in := []Entry{
		{Key: 2, NodeID: id2, Name: "b1"},
		{Key: 1, NodeID: id1, Name: "a1"},
		{Key: 9, NodeID: id1}, // unnamed (RPC-tier identity)
	}
	if err := WriteManifest(dir, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 || out[0].Key != 1 || out[0].Name != "a1" || out[0].NodeID != id1 ||
		out[1].Key != 2 || out[1].Name != "b1" || out[2].Key != 9 || out[2].Name != "" {
		t.Fatalf("ReadManifest = %+v", out)
	}
}
