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
	"flag"
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
		fs := flag.NewFlagSet("system-start", flag.ExitOnError)
		archStr := fs.String("arch", "5+0", "validator placement: N+M where N+M=5 (e.g. 5+0, 4+1, 3+2)")
		if err := fs.Parse(os.Args[2:]); err != nil {
			os.Exit(2)
		}
		arch, parseErr := parseArch(*archStr)
		if parseErr != nil {
			fmt.Fprintln(os.Stderr, "ERROR:", parseErr)
			os.Exit(2)
		}
		err = systemStart(ctx, arch)
	case "kill-dc1":
		err = killDC1(ctx)
	case "dc2-takeover":
		err = dc2Takeover(ctx)
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
	fmt.Fprintln(os.Stderr, "  failover system-start [--arch N+M]  boot the lab; default --arch=5+0")
	fmt.Fprintln(os.Stderr, "  failover kill-dc1                   pkill avalanchego on the 5 DC1 hosts")
	fmt.Fprintln(os.Stderr, "  failover dc2-takeover               key-swap DC1 staking onto DC2 and restart")
}
