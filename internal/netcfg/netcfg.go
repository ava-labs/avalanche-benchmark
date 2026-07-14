// Package netcfg resolves which Avalanche network the kit targets (fuji or
// mainnet) into the per-network values every tool needs. Selection order:
// NETWORK (persisted in network.env by `l1 create`: the network is a property
// of the created chain), then AVALANCHE_NETWORK (bootstrap-time: the setup
// scripts' --mainnet flag exports it before network.env exists), then fuji.
// Individual values keep their pre-existing env overrides (PCHAIN_API,
// FUJI_UPSTREAM_IPS/IDS).
package netcfg

import (
	"fmt"
	"os"
	"sync"

	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/units"
)

// Config is the resolved per-network configuration.
type Config struct {
	Name      string // "fuji" | "mainnet"; also the avalanchego --network-id value
	NetworkID uint32 // warp / P-chain wallet network ID
	HRP       string // bech32 HRP for P-chain address formatting
	// API is the public API base (info + platform.*). The kit's own
	// follow-only RPC tier never serves platform.*, so P-chain reads and txs
	// (cmd/l1, fuji-wallet, the fleet's status/exporter reads) go here.
	// Its backends are load-balanced and can serve stale reads; internal/vset
	// retries around that. Env override: PCHAIN_API.
	API string
	// UpstreamIPs/IDs: the ONE public bootstrap peer the RPC tier's P-chain
	// follows (first entry of the pinned avalanchego commit's
	// genesis/bootstrappers.json for this network; rotates on
	// AVALANCHEGO_COMMIT bumps). Env overrides: FUJI_UPSTREAM_IPS /
	// FUJI_UPSTREAM_IDS (names kept for .env compatibility on both networks).
	UpstreamIPs string
	UpstreamIDs string
	// ValidatorBalance is the per-validator continuous-fee deposit paid by
	// ConvertSubnetToL1Tx, in nAVAX. Fuji: 0.1 AVAX lasts ~5-6 days. Mainnet:
	// 0.15 AVAX covers ~3 days at the 512 nAVAX/s fee floor plus margin (the
	// mainnet benchmark network is disposable; extend with `fuji-wallet topup`).
	ValidatorBalance uint64
}

var fuji = Config{
	Name:             "fuji",
	NetworkID:        constants.FujiID,
	HRP:              constants.FujiHRP,
	API:              "https://api.avax-test.network",
	UpstreamIPs:      "18.192.93.241:9651",
	UpstreamIDs:      "NodeID-2m38qc95mhHXtrhjyGbe7r2NhniqHHJRB",
	ValidatorBalance: 100 * units.MilliAvax,
}

var mainnet = Config{
	Name:             "mainnet",
	NetworkID:        constants.MainnetID,
	HRP:              constants.MainnetHRP,
	API:              "https://api.avax.network",
	UpstreamIPs:      "54.232.137.108:9651",
	UpstreamIDs:      "NodeID-A6onFGyJjA37EZ7kYHANMR1PFRT8NmXrF",
	ValidatorBalance: 150 * units.MilliAvax,
}

// Resolve builds the Config from the given env lookup (injectable for tests).
func Resolve(getenv func(string) string) (Config, error) {
	name := getenv("NETWORK")
	if name == "" {
		name = getenv("AVALANCHE_NETWORK")
	}
	var c Config
	switch name {
	case "", "fuji":
		c = fuji
	case "mainnet":
		c = mainnet
	default:
		return Config{}, fmt.Errorf("unknown network %q (NETWORK / AVALANCHE_NETWORK): want fuji or mainnet", name)
	}
	for _, o := range []struct {
		env string
		dst *string
	}{
		{"PCHAIN_API", &c.API},
		{"FUJI_UPSTREAM_IPS", &c.UpstreamIPs},
		{"FUJI_UPSTREAM_IDS", &c.UpstreamIDs},
	} {
		if v := getenv(o.env); v != "" {
			*o.dst = v
		}
	}
	return c, nil
}

var (
	once sync.Once
	cfg  Config
)

// Get resolves once from the process env. Call sites all run after their
// tool's godotenv loads, so NETWORK from network.env is visible. An unknown
// network name is fatal: no consumer can do anything sensible without one.
func Get() Config {
	once.Do(func() {
		var err error
		if cfg, err = Resolve(os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	})
	return cfg
}
