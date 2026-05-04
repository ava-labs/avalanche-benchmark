// failover is the runtime orchestrator for the 2-DC failover lab.
//
// Subcommands:
//
//	failover system-start    boot the lab end-to-end and print RPC URLs
//	failover kill-dc1        (TODO) pkill avalanchego on the 5 DC1 hosts
//	failover dc2-takeover    (TODO) kill DC1, key-swap onto DC2, restart
//
// All subcommands run on the control node. State that downstream commands
// need (SUBNET_ID, CHAIN_ID) is persisted to ./network.env after
// system-start.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	var err error
	switch os.Args[1] {
	case "system-start":
		err = systemStart(ctx)
	case "kill-dc1", "dc2-takeover":
		err = fmt.Errorf("subcommand %q is not implemented yet", os.Args[1])
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  failover system-start     boot the lab and print RPC URLs")
	fmt.Fprintln(os.Stderr, "  failover kill-dc1         (todo) kill avalanchego on the 5 DC1 hosts")
	fmt.Fprintln(os.Stderr, "  failover dc2-takeover     (todo) key-swap onto DC2 and restart")
}
