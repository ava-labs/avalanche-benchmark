package netcfg

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/rpc"
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

// TestAPITokenQueryParam: with the transport installed, an avalanchego rpc
// request to the API host arrives with token=<value> in the query, and a
// request to any other host arrives without it.
func TestAPITokenQueryParam(t *testing.T) {
	var gotToken string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotToken = r.URL.Query().Get("token")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}
	api := httptest.NewServer(http.HandlerFunc(handler))
	defer api.Close()
	other := httptest.NewServer(http.HandlerFunc(handler))
	defer other.Close()

	saved := http.DefaultClient.Transport
	defer func() { http.DefaultClient.Transport = saved }()
	installAPIToken(api.URL, "sekrit")

	send := func(base string) {
		t.Helper()
		u, _ := url.Parse(base + "/ext/P")
		var reply struct{}
		if err := rpc.SendJSONRequest(context.Background(), u, "platform.getHeight", struct{}{}, &reply); err != nil {
			t.Fatalf("SendJSONRequest(%s): %v", base, err)
		}
	}

	send(api.URL)
	if gotToken != "sekrit" {
		t.Errorf("API host request: token = %q, want %q", gotToken, "sekrit")
	}
	send(other.URL)
	if gotToken != "" {
		t.Errorf("other host request: token = %q, want empty", gotToken)
	}
}

// TestAPITokenUnsetNoop: empty token must leave http.DefaultClient alone.
func TestAPITokenUnsetNoop(t *testing.T) {
	saved := http.DefaultClient.Transport
	defer func() { http.DefaultClient.Transport = saved }()
	installAPIToken("https://api.avax.network", "")
	if http.DefaultClient.Transport != saved {
		t.Error("transport replaced despite empty token")
	}
}
