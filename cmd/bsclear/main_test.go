package main

import (
	"testing"

	"github.com/cockroachdb/pebble"

	"github.com/ava-labs/avalanchego/database/pebbledb"
	"github.com/ava-labs/avalanchego/database/prefixdb"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/logging"
)

// TestClearDropsOnlyTheChainsBootstrapBacklog builds a store through the SAME
// layering avalanchego's chains/manager uses (prefixdb.New(chainID[:]) then
// prefixdb.New("interval_bs")), plus sibling data that must survive: the same
// chain's vm prefix and another chain's (stand-in for the P-chain) bootstrap
// prefix. clear() must remove exactly the target chain's backlog.
func TestClearDropsOnlyTheChainsBootstrapBacklog(t *testing.T) {
	dir := t.TempDir() + "/pebble"
	l1 := ids.GenerateTestID()
	pchain := ids.GenerateTestID()

	db, err := pebbledb.New(dir, nil, logging.NoLog{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	l1DB := prefixdb.New(l1[:], db)
	l1BS := prefixdb.New(chainBootstrappingDBPrefix, l1DB)
	l1VM := prefixdb.New([]byte("vm"), l1DB)
	pBS := prefixdb.New(chainBootstrappingDBPrefix, prefixdb.New(pchain[:], db))
	for _, kv := range []struct {
		db  interface{ Put([]byte, []byte) error }
		key string
	}{
		{l1BS, "block-1"}, {l1BS, "block-2"}, {l1BS, "tree"},
		{l1VM, "state"}, {pBS, "p-block"},
	} {
		if err := kv.db.Put([]byte(kv.key), []byte("v")); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := clear(dir, l1); err != nil {
		t.Fatal(err)
	}

	// Reopen raw and check survivors/casualties by raw key.
	raw, err := pebble.Open(dir, &pebble.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	l1bsPrefix, _ := bootstrapPrefix(l1)
	pbsPrefix, _ := bootstrapPrefix(pchain)
	vmPrefix := prefixdb.JoinPrefixes(prefixdb.MakePrefix(l1[:]), []byte("vm"))
	has := func(prefix []byte, key string) bool {
		_, closer, err := raw.Get(append(append([]byte(nil), prefix...), key...))
		if err == pebble.ErrNotFound {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		closer.Close()
		return true
	}
	for _, key := range []string{"block-1", "block-2", "tree"} {
		if has(l1bsPrefix, key) {
			t.Errorf("L1 bootstrap key %q survived the clear", key)
		}
	}
	if !has(vmPrefix, "state") {
		t.Errorf("the chain's own vm data was deleted - prefix too wide")
	}
	if !has(pbsPrefix, "p-block") {
		t.Errorf("ANOTHER chain's bootstrap data was deleted - prefix too wide")
	}

	// A missing dir is a clean no-op.
	if err := clear(dir+"-missing", l1); err != nil {
		t.Errorf("missing dir should be a no-op, got %v", err)
	}
}
