// Command bsclear drops the L1 chain's bootstrap-block backlog from a node's
// shared avalanchego database. Run ON the node, only while avalanchego is DOWN.
//
// Why: bootstrap-fetched blocks live in the shared db/ (NOT in chainData/), so
// a chainData-only rebuild resurrects a half-finished bootstrap, and on the
// next start avalanchego grinds through an UNLOGGED Bootstrapper.Clear
// (database.AtomicClear, key-by-key) of that backlog before "starting state
// sync" can even appear - minutes of silence that look exactly like a frozen
// node (2026-07-11 incident). One pebble DeleteRange tombstones the whole
// prefix in O(1) instead.
//
// Key layout (avalanchego chains/manager.go): the chain's db is
// prefixdb.New(chainID[:]) over the node db, and the bootstrap storage is
// prefixdb.New(ChainBootstrappingDBPrefix, chainDB), so the raw keys are
// sha256(sha256(chainID) || "interval_bs") || key. Every other chain -
// including the P-chain - lives under a disjoint sha256 prefix and is
// untouched; staking keys are not in the db at all.
package main

import (
	"fmt"
	"os"

	"github.com/cockroachdb/pebble"

	"github.com/ava-labs/avalanchego/database/prefixdb"
	"github.com/ava-labs/avalanchego/ids"
)

// chainBootstrappingDBPrefix mirrors avalanchego chains/manager.go
// ChainBootstrappingDBPrefix (unexported mirror rather than importing the
// whole chains package for one 11-byte constant).
var chainBootstrappingDBPrefix = []byte("interval_bs")

// bootstrapPrefix computes the raw key prefix of the chain's bootstrap storage
// and the lexically-one-past limit of its range (a sha256 output is never all
// 0xff, so the increment cannot overflow off the front).
func bootstrapPrefix(chainID ids.ID) (prefix, limit []byte) {
	prefix = prefixdb.JoinPrefixes(prefixdb.MakePrefix(chainID[:]), chainBootstrappingDBPrefix)
	limit = append([]byte(nil), prefix...)
	for i := len(limit) - 1; i >= 0; i-- {
		limit[i]++
		if limit[i] != 0 {
			break
		}
	}
	return prefix, limit
}

// clear opens the pebble store at dir and range-deletes the chain's bootstrap
// backlog. A missing dir is a no-op (fresh box, nothing fetched yet).
func clear(dir string, chainID ids.ID) error {
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		fmt.Printf("bsclear: %s does not exist, nothing to clear\n", dir)
		return nil
	}
	// Same pinned pebble version as the node binary; avalanchego's wrapper uses
	// default comparer/merger, so default options open the store fine.
	db, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		return fmt.Errorf("open %s: %w", dir, err)
	}
	prefix, limit := bootstrapPrefix(chainID)
	if err := db.DeleteRange(prefix, limit, pebble.Sync); err != nil {
		db.Close()
		return fmt.Errorf("delete range: %w", err)
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	fmt.Printf("bsclear: dropped bootstrap backlog of chain %s (prefix %x)\n", chainID, prefix)
	return nil
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: bsclear <pebble-db-dir> <chainID>\nMUST run with avalanchego stopped.")
		os.Exit(2)
	}
	cid, err := ids.FromString(os.Args[2])
	if err != nil {
		fmt.Fprintf(os.Stderr, "bsclear: bad chainID %q: %v\n", os.Args[2], err)
		os.Exit(1)
	}
	if err := clear(os.Args[1], cid); err != nil {
		fmt.Fprintf(os.Stderr, "bsclear: %v\n", err)
		os.Exit(1)
	}
}
