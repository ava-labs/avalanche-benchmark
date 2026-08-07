package topup

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/internal/weights"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	commonopts "github.com/ava-labs/avalanchego/wallet/subnet/primary/common"
)

type fakeWallet struct {
	wallet.Wallet
	amounts []uint64
	txIDs   []ids.ID
}

func (f *fakeWallet) IssueIncreaseL1ValidatorBalanceTx(_ ids.ID, amount uint64, _ ...commonopts.Option) (*txs.Tx, error) {
	id := ids.GenerateTestID()
	f.amounts = append(f.amounts, amount)
	f.txIDs = append(f.txIDs, id)
	return &txs.Tx{TxID: id}, nil
}

func TestPlanAndExecuteReportsTransactionOrExistingDaysForEveryValidator(t *testing.T) {
	price := uint64(10)
	target, err := targetBalance(2, price)
	if err != nil {
		t.Fatal(err)
	}
	validators := []weights.Validator{
		{L1: "management", NodeID: ids.GenerateTestNodeID(), ValidationID: ids.GenerateTestID(), Balance: target},
		{L1: "main", NodeID: ids.GenerateTestNodeID(), ValidationID: ids.GenerateTestID(), Balance: target - 100},
		{L1: "main", NodeID: ids.GenerateTestNodeID(), ValidationID: ids.GenerateTestID(), Balance: target + 100},
	}
	topupTarget := target + settlementBuffer*price
	actions, total, err := plan(validators, target, topupTarget)
	if err != nil {
		t.Fatal(err)
	}
	wantShortfall := settlementBuffer*price + 100
	if total != wantShortfall {
		t.Fatalf("unexpected total shortfall %d", total)
	}
	wallet := &fakeWallet{}
	var output bytes.Buffer
	if err := execute(wallet, actions, price, &output); err != nil {
		t.Fatal(err)
	}
	if len(wallet.amounts) != 1 || wallet.amounts[0] != wantShortfall {
		t.Fatalf("unexpected topups: %v", wallet.amounts)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != len(validators) {
		t.Fatalf("expected one line per validator, got %q", output.String())
	}
	if !strings.Contains(lines[0], "already had 2.00 days") || !strings.Contains(lines[1], wallet.txIDs[0].String()) || !strings.Contains(lines[2], "already had 2.00 days") {
		t.Fatalf("unexpected output %q", output.String())
	}
}

func TestTargetBalanceRejectsZeroPriceAndOverflow(t *testing.T) {
	if _, err := targetBalance(1, 0); err == nil {
		t.Fatal("zero fee price must fail")
	}
	if _, err := targetBalance(^uint64(0), 1); err == nil {
		t.Fatal("overflowing days must fail")
	}
}
