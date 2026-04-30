package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ava-labs/avalanche-benchmark/rpc-gateway/internal/config"
	"github.com/ava-labs/avalanche-benchmark/rpc-gateway/internal/gateway"
	"github.com/ava-labs/avalanche-benchmark/rpc-gateway/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{}))

	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		rawKey, hash, err := store.GenerateAPIKey()
		if err != nil {
			logger.Error("failed to generate API key", "error", err)
			os.Exit(1)
		}

		fmt.Printf("raw_key=%s\n", rawKey)
		fmt.Printf("sha256=%s\n", hash)
		return
	}
	if len(os.Args) > 2 && os.Args[1] == "hash-key" {
		fmt.Printf("sha256=%s\n", store.HashAPIKey(os.Args[2]))
		return
	}

	cfg, err := config.Load()
	if err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.NewPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		logger.Error("failed to connect to Postgres", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	app, err := gateway.New(ctx, logger, cfg, db)
	if err != nil {
		logger.Error("failed to initialize gateway", "error", err)
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("gateway shutdown failed", "error", err)
		}
	}()

	logger.Info(
		"rpc gateway listening",
		"listen_addr", cfg.ListenAddr,
		"upstream_rpc_url", cfg.UpstreamRPCURL,
		"upstream_chain_id", app.UpstreamChainID(),
	)

	err = server.ListenAndServe()
	if err != nil && err != http.ErrServerClosed {
		logger.Error("gateway server failed", "error", err)
		os.Exit(1)
	}
}
