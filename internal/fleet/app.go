package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
)

// appManifest is the contract between an app and the installer. Each app
// declares it in apps/<name>/app.json. The manifest names a renderer, not an
// upgrade: the renderer runs at install time, from the deployment root, and
// writes a fresh fragment with a future activation timestamp. A checked-in
// fragment would go stale the moment its timestamp passed.
type appManifest struct {
	// Name must equal the app's directory name under apps/.
	Name string `json:"name"`
	// Description is one line for fleet app list.
	Description string `json:"description"`
	// Chain is the default target chain. Empty means main. The --chain flag
	// overrides it at install time.
	Chain string `json:"chain,omitempty"`
	// Render is the argv that writes the upgrade fragment. It runs from the
	// deployment root and inherits the environment.
	Render []string `json:"render"`
	// Output is the file the renderer writes, relative to the deployment
	// root. Empty means upgrade.json.
	Output string `json:"output,omitempty"`
}

// appManifestPath returns apps/<name>/app.json under the deployment root.
func appManifestPath(root, name string) string {
	return filepath.Join(root, "apps", name, "app.json")
}

// parseAppManifest decodes and validates a manifest for the app directory
// named dir. It applies the defaults, so a caller never sees an empty Output.
func parseAppManifest(contents []byte, dir string) (appManifest, error) {
	var manifest appManifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return appManifest{}, fmt.Errorf("apps/%s/app.json is not valid JSON: %w", dir, err)
	}
	if manifest.Name != dir {
		return appManifest{}, fmt.Errorf("apps/%s/app.json names the app %q; the name must equal the directory name", dir, manifest.Name)
	}
	if len(manifest.Render) == 0 || manifest.Render[0] == "" {
		return appManifest{}, fmt.Errorf("apps/%s/app.json has an empty render command", dir)
	}
	if manifest.Chain != "" && !config.ValidChainName(manifest.Chain) {
		return appManifest{}, fmt.Errorf("apps/%s/app.json chain %q is not a valid chain name (lowercase letters, digits, hyphens, at most 20)", dir, manifest.Chain)
	}
	if manifest.Output == "" {
		manifest.Output = "upgrade.json"
	}
	if filepath.IsAbs(manifest.Output) || strings.Contains(manifest.Output, "..") {
		return appManifest{}, fmt.Errorf("apps/%s/app.json output %q must be a plain path relative to the deployment root", dir, manifest.Output)
	}
	return manifest, nil
}

// loadAppManifest reads and validates apps/<name>/app.json.
func loadAppManifest(root, name string) (appManifest, error) {
	contents, err := os.ReadFile(appManifestPath(root, name))
	if err != nil {
		return appManifest{}, err
	}
	return parseAppManifest(contents, name)
}

// resolveAppChain picks the one chain an install targets: the --chain flag
// wins, then the manifest's chain, then main. An install never targets more
// than one chain; the manifest and the flag both name exactly one.
func resolveAppChain(flag, manifest string) string {
	if flag != "" {
		return flag
	}
	if manifest != "" {
		return manifest
	}
	return config.MainChain
}

// requireDeclaredChain refuses a target chain that nodes.ini does not
// declare, before the renderer runs and before any remote work.
func requireDeclaredChain(declared []string, chain string) error {
	for _, name := range declared {
		if name == chain {
			return nil
		}
	}
	return fmt.Errorf("chain %q is not declared in nodes.ini; declared chains: %s", chain, strings.Join(declared, " "))
}

// InstallApp installs one app onto one running chain. It reads the app's
// manifest, resolves the target chain, runs the app's renderer from the
// deployment root, and hands the rendered fragment to the same code path as
// fleet upgrade: append to the chain's history, push, rolling restart.
func (d *Deployer) InstallApp(ctx context.Context, name, chainFlag string) error {
	manifest, err := loadAppManifest(d.root, name)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("app %q has no manifest; expected %s", name, appManifestPath(d.root, name))
		}
		return err
	}
	chain := resolveAppChain(chainFlag, manifest.Chain)

	// The chain check runs first, so a wrong --chain fails before the
	// renderer writes anything.
	inv, err := d.inventory()
	if err != nil {
		return err
	}
	if err := requireDeclaredChain(inv.chains, chain); err != nil {
		return err
	}

	fmt.Fprintf(d.out, "rendering %s: %s\n", name, strings.Join(manifest.Render, " "))
	render := exec.CommandContext(ctx, manifest.Render[0], manifest.Render[1:]...)
	render.Dir = d.root
	render.Env = os.Environ()
	render.Stdout = d.out
	render.Stderr = os.Stderr
	if err := render.Run(); err != nil {
		return fmt.Errorf("render command %q failed: %w", strings.Join(manifest.Render, " "), err)
	}

	output := filepath.Join(d.root, manifest.Output)
	if _, err := os.Stat(output); err != nil {
		return fmt.Errorf("the renderer did not write %s: %w", output, err)
	}
	fmt.Fprintf(d.out, "rendered %s; installing on chain %q\n", output, chain)

	if err := d.UpgradeChain(ctx, chain, output); err != nil {
		return err
	}
	fmt.Fprintf(d.out, "app %s installed on chain %q\n", name, chain)
	fmt.Fprintln(d.out, "next steps:")
	fmt.Fprintln(d.out, "  1. The chain applies the upgrade in the first block at or after the activation time.")
	fmt.Fprintln(d.out, "  2. Send one transaction after the activation time; an idle chain produces no block on its own.")
	fmt.Fprintln(d.out, "  3. Verify the app's contracts, for example with a cast call against a fixed address.")
	return nil
}

// ListApps prints one line per app under apps/: name, target chain, and
// description. A directory without app.json is skipped with a warning, so an
// app that predates the manifest convention does not break the listing.
func (d *Deployer) ListApps() error {
	appsDir := filepath.Join(d.root, "apps")
	entries, err := os.ReadDir(appsDir)
	if err != nil {
		return fmt.Errorf("read %s: %w", appsDir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		manifest, err := loadAppManifest(d.root, entry.Name())
		if os.IsNotExist(err) {
			fmt.Fprintf(d.out, "warning: apps/%s has no app.json; skipped\n", entry.Name())
			continue
		}
		if err != nil {
			return err
		}
		fmt.Fprintf(d.out, "%-24s chain=%-8s %s\n", manifest.Name, resolveAppChain("", manifest.Chain), manifest.Description)
	}
	return nil
}
