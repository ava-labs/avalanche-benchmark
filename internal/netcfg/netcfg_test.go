package netcfg

import (
	"testing"

	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/units"
)

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// TestFujiDefaultsUnchanged pins every fuji default to the exact values the
// kit shipped with before the network switch existed: an empty env must
// resolve to precisely the old behavior.
func TestFujiDefaultsUnchanged(t *testing.T) {
	for _, name := range []string{"", "fuji"} {
		c, err := Resolve(env(map[string]string{"AVALANCHE_NETWORK": name}))
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		want := Config{
			Name:             "fuji",
			NetworkID:        constants.FujiID,
			HRP:              constants.FujiHRP,
			API:              "https://api.avax-test.network",
			UpstreamIPs:      "18.192.93.241:9651",
			UpstreamIDs:      "NodeID-2m38qc95mhHXtrhjyGbe7r2NhniqHHJRB",
			ValidatorBalance: 100 * units.MilliAvax,
		}
		if c != want {
			t.Errorf("Resolve(%q) = %+v, want %+v", name, c, want)
		}
	}
}

func TestMainnet(t *testing.T) {
	c, err := Resolve(env(map[string]string{"AVALANCHE_NETWORK": "mainnet"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "mainnet" || c.NetworkID != constants.MainnetID || c.HRP != constants.MainnetHRP {
		t.Errorf("identity fields: %+v", c)
	}
	if c.API != "https://api.avax.network" {
		t.Errorf("endpoint fields: %+v", c)
	}
	if c.ValidatorBalance != 150*units.MilliAvax {
		t.Errorf("ValidatorBalance = %d", c.ValidatorBalance)
	}
}

// TestNetworkEnvWinsOverAvalancheNetwork: NETWORK (from network.env, the
// created chain's own record) beats the shell's AVALANCHE_NETWORK.
func TestNetworkEnvWinsOverAvalancheNetwork(t *testing.T) {
	c, err := Resolve(env(map[string]string{"NETWORK": "mainnet", "AVALANCHE_NETWORK": "fuji"}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Name != "mainnet" {
		t.Errorf("Name = %q, want mainnet", c.Name)
	}
}

func TestEnvOverrides(t *testing.T) {
	c, err := Resolve(env(map[string]string{
		"AVALANCHE_NETWORK": "mainnet",
		"PCHAIN_API":        "http://api.example",
		"FUJI_UPSTREAM_IPS": "1.2.3.4:9651",
		"FUJI_UPSTREAM_IDS": "NodeID-x",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.API != "http://api.example" || c.UpstreamIPs != "1.2.3.4:9651" ||
		c.UpstreamIDs != "NodeID-x" {
		t.Errorf("overrides not applied: %+v", c)
	}
	if c.NetworkID != constants.MainnetID {
		t.Errorf("NetworkID = %d", c.NetworkID)
	}
}

func TestUnknownNetwork(t *testing.T) {
	if _, err := Resolve(env(map[string]string{"AVALANCHE_NETWORK": "testnet"})); err == nil {
		t.Fatal("want error for unknown network")
	}
}
