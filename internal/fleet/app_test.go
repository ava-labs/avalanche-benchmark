package fleet

import (
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
)

// The manifest is the whole contract between an app and the installer, so
// the parse must catch every declaration mistake before any command runs.
func TestParseAppManifest(t *testing.T) {
	for name, testCase := range map[string]struct {
		contents string
		dir      string
		wantErr  string
	}{
		"valid manifest": {
			contents: `{"name":"feed","description":"a feed","chain":"main","render":["./bin/oracle","upgrade"],"output":"upgrade.json"}`,
			dir:      "feed",
		},
		"name mismatch": {
			contents: `{"name":"other","render":["./bin/oracle","upgrade"]}`,
			dir:      "feed",
			wantErr:  "must equal the directory name",
		},
		"empty render": {
			contents: `{"name":"feed","render":[]}`,
			dir:      "feed",
			wantErr:  "empty render command",
		},
		"blank render entry": {
			contents: `{"name":"feed","render":[""]}`,
			dir:      "feed",
			wantErr:  "empty render command",
		},
		"bad chain name": {
			contents: `{"name":"feed","chain":"Main Chain!","render":["./bin/oracle"]}`,
			dir:      "feed",
			wantErr:  "not a valid chain name",
		},
		"absolute output": {
			contents: `{"name":"feed","render":["./bin/oracle"],"output":"/etc/upgrade.json"}`,
			dir:      "feed",
			wantErr:  "relative to the deployment root",
		},
		"not json": {
			contents: `render: oracle`,
			dir:      "feed",
			wantErr:  "not valid JSON",
		},
	} {
		t.Run(name, func(t *testing.T) {
			manifest, err := parseAppManifest([]byte(testCase.contents), testCase.dir)
			if testCase.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if manifest.Name != testCase.dir {
					t.Fatalf("name = %q, want %q", manifest.Name, testCase.dir)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error containing %q, got none", testCase.wantErr)
			}
			if !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("error %q does not contain %q", err, testCase.wantErr)
			}
		})
	}
}

// A manifest that omits the optional fields gets the documented defaults.
func TestParseAppManifestDefaults(t *testing.T) {
	manifest, err := parseAppManifest([]byte(`{"name":"feed","render":["./bin/oracle","upgrade"]}`), "feed")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Output != "upgrade.json" {
		t.Fatalf("output = %q, want the default upgrade.json", manifest.Output)
	}
	if manifest.Chain != "" {
		t.Fatalf("chain = %q, want empty (main resolves later)", manifest.Chain)
	}
}

// The install targets exactly one chain: the flag wins, then the manifest,
// then main. This ordering is the whole command-line contract.
func TestResolveAppChain(t *testing.T) {
	for name, testCase := range map[string]struct {
		flag     string
		manifest string
		want     string
	}{
		"flag wins over manifest": {flag: "alt", manifest: "feedchain", want: "alt"},
		"manifest wins over main": {manifest: "feedchain", want: "feedchain"},
		"main is the default":     {want: config.MainChain},
		"flag alone":              {flag: "alt", want: "alt"},
	} {
		t.Run(name, func(t *testing.T) {
			got := resolveAppChain(testCase.flag, testCase.manifest)
			if got != testCase.want {
				t.Fatalf("resolveAppChain(%q, %q) = %q, want %q", testCase.flag, testCase.manifest, got, testCase.want)
			}
		})
	}
}

// An undeclared chain is refused before the renderer runs and before any
// remote work, with the declared chains named in the error.
func TestRequireDeclaredChain(t *testing.T) {
	declared := []string{"main", "alt"}
	if err := requireDeclaredChain(declared, "alt"); err != nil {
		t.Fatalf("declared chain refused: %v", err)
	}
	err := requireDeclaredChain(declared, "ghost")
	if err == nil {
		t.Fatal("undeclared chain accepted")
	}
	for _, want := range []string{"ghost", "nodes.ini", "main alt"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
}
