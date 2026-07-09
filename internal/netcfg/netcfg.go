// Package netcfg resolves which Avalanche network the kit targets (fuji or
// mainnet) into the per-network values every tool needs. Selection order:
// NETWORK (persisted in network.env by create-l1: the network is a property
// of the created chain), then AVALANCHE_NETWORK (bootstrap-time: the setup
// scripts' --mainnet flag exports it before network.env exists), then fuji.
// Individual values keep their pre-existing env overrides (PCHAIN_API,
// CCHAIN_RPC, PCHAIN_RPC, AGGREGATOR_URL, GLACIER_URL, FUJI_UPSTREAM_IPS/IDS).
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
	// API is the public API base (info + platform.* + C-chain). The kit's own
	// follow-only RPC tier never serves platform.*, so P-chain txs go here.
	// Env override: PCHAIN_API.
	API string
	// CChainRPC / PChainRPC are the publicnode per-chain RPCs the fleet-side
	// tools use (the official API aggressively rate-limits the fleet's egress
	// IP). Env overrides: CCHAIN_RPC / PCHAIN_RPC.
	CChainRPC string
	PChainRPC string
	// CChainID is the network's C-chain blockchain ID (the ValidatorManager's
	// chain and warp source chain), hardcoded because publicnode does not
	// serve /ext/info. Mainnet value verified against api.avax.network
	// info.getBlockchainID on 2026-07-09.
	CChainID string
	// AggregatorURL is the primary signature aggregator (our own fly.io
	// deployment, aggregates fresh per request); GlacierURL is the cached
	// public fallback. Env overrides: AGGREGATOR_URL / GLACIER_URL.
	AggregatorURL string
	GlacierURL    string
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
	CChainRPC:        "https://avalanche-fuji-c-chain-rpc.publicnode.com",
	PChainRPC:        "https://avalanche-fuji-p-chain-rpc.publicnode.com/ext/bc/P",
	CChainID:         "yH8D7ThNJkxmtkuv2jgBa4P1Rn3Qpr4pPr7QYNfcdoS6k6HWp",
	AggregatorURL:    "https://avax-signature-aggregator-fuji.fly.dev/aggregate-signatures",
	GlacierURL:       "https://glacier-api.avax.network/v1/signatureAggregator/fuji/aggregateSignatures",
	UpstreamIPs:      "18.192.93.241:9651",
	UpstreamIDs:      "NodeID-2m38qc95mhHXtrhjyGbe7r2NhniqHHJRB",
	ValidatorBalance: 100 * units.MilliAvax,
}

var mainnet = Config{
	Name:             "mainnet",
	NetworkID:        constants.MainnetID,
	HRP:              constants.MainnetHRP,
	API:              "https://api.avax.network",
	CChainRPC:        "https://avalanche-c-chain-rpc.publicnode.com",
	PChainRPC:        "https://avalanche-p-chain-rpc.publicnode.com/ext/bc/P",
	CChainID:         "2q9e4r6Mu3U68nU1fYjgbR6JvwrRx36CohpAX5UQxse55x1Q5",
	AggregatorURL:    "https://avax-signature-aggregator-mainnet.fly.dev/aggregate-signatures",
	GlacierURL:       "https://glacier-api.avax.network/v1/signatureAggregator/mainnet/aggregateSignatures",
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
		{"CCHAIN_RPC", &c.CChainRPC},
		{"PCHAIN_RPC", &c.PChainRPC},
		{"AGGREGATOR_URL", &c.AggregatorURL},
		{"GLACIER_URL", &c.GlacierURL},
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
