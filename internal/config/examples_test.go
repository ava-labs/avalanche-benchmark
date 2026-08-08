package config

import (
	"path/filepath"
	"testing"
)

// TestExampleInventoriesLoad pins every shipped example inventory to the
// parser. An example that stops loading is a release defect: a new user's
// first command is a copy of one of these files.
func TestExampleInventoriesLoad(t *testing.T) {
	examples, err := filepath.Glob(filepath.Join("..", "..", "examples", "*.ini"))
	if err != nil {
		t.Fatal(err)
	}
	examples = append(examples, filepath.Join("..", "..", "examples", "nodes.ini.example"))
	if len(examples) < 5 {
		t.Fatalf("expected at least 5 example inventories, found %d: %v", len(examples), examples)
	}
	for _, path := range examples {
		nodes, err := LoadNodes(path)
		if err != nil {
			t.Errorf("%s does not load: %v", path, err)
			continue
		}
		if len(nodes) == 0 {
			t.Errorf("%s loads no nodes", path)
		}
	}
}
