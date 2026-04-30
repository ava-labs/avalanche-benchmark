package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultListenAddr    = ":8080"
	defaultRequestTimout = 15 * time.Second
)

type Config struct {
	ListenAddr          string
	DatabaseURL         string
	UpstreamRPCURL      string
	BenchmarkDataDir    string
	TrustForwardedFor   bool
	RequestTimeout      time.Duration
	MaxRequestBodyBytes int64
}

func Load() (Config, error) {
	cfg := Config{
		ListenAddr:          getenv("LISTEN_ADDR", defaultListenAddr),
		DatabaseURL:         strings.TrimSpace(os.Getenv("DATABASE_URL")),
		UpstreamRPCURL:      strings.TrimSpace(os.Getenv("UPSTREAM_RPC_URL")),
		BenchmarkDataDir:    strings.TrimSpace(os.Getenv("BENCHMARK_DATA_DIR")),
		TrustForwardedFor:   strings.EqualFold(strings.TrimSpace(os.Getenv("TRUST_X_FORWARDED_FOR")), "true"),
		RequestTimeout:      defaultRequestTimout,
		MaxRequestBodyBytes: 1 << 20,
	}

	if cfg.DatabaseURL == "" {
		return Config{}, fmt.Errorf("DATABASE_URL is required")
	}

	if raw := strings.TrimSpace(os.Getenv("REQUEST_TIMEOUT")); raw != "" {
		timeout, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("invalid REQUEST_TIMEOUT: %w", err)
		}
		cfg.RequestTimeout = timeout
	}

	if cfg.UpstreamRPCURL == "" {
		url, err := resolveUpstreamRPCURL(cfg.BenchmarkDataDir)
		if err != nil {
			return Config{}, err
		}
		cfg.UpstreamRPCURL = url
	}

	return cfg, nil
}

func resolveUpstreamRPCURL(configuredDataDir string) (string, error) {
	candidates := []string{}
	if configuredDataDir != "" {
		candidates = append(candidates, configuredDataDir)
	}
	candidates = append(candidates,
		"./single-node/network_data",
		"../single-node/network_data",
		"./network_data",
		"../network_data",
	)

	for _, dir := range candidates {
		if dir == "" {
			continue
		}

		rpcsPath := filepath.Join(dir, "rpcs.txt")
		data, err := os.ReadFile(rpcsPath)
		if err != nil {
			continue
		}

		urls := strings.Split(strings.TrimSpace(string(data)), ",")
		for _, url := range urls {
			url = strings.TrimSpace(url)
			if url != "" {
				return url, nil
			}
		}
	}

	return "", fmt.Errorf("UPSTREAM_RPC_URL is not set and no RPC URL was found in network_data/rpcs.txt")
}

func getenv(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
