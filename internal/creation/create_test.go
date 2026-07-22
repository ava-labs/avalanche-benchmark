package creation

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/keychain"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	commonopts "github.com/ava-labs/avalanchego/wallet/subnet/primary/common"
)

type conversionCall struct {
	subnetID  ids.ID
	managerID ids.ID
	address   []byte
	values    []*txs.ConvertSubnetToL1Validator
}

type fakeWallet struct {
	pwallet.Wallet
	events      []string
	txIDs       []ids.ID
	conversions []conversionCall
}

func (w *fakeWallet) next(event string) *txs.Tx {
	w.events = append(w.events, event)
	id := ids.GenerateTestID()
	w.txIDs = append(w.txIDs, id)
	return &txs.Tx{TxID: id}
}

func (w *fakeWallet) IssueCreateSubnetTx(*secp256k1fx.OutputOwners, ...commonopts.Option) (*txs.Tx, error) {
	return w.next("create-subnet"), nil
}

func (w *fakeWallet) IssueCreateChainTx(ids.ID, []byte, ids.ID, []ids.ID, string, ...commonopts.Option) (*txs.Tx, error) {
	return w.next("create-chain"), nil
}

func (w *fakeWallet) IssueConvertSubnetToL1Tx(
	subnetID ids.ID,
	managerID ids.ID,
	address []byte,
	values []*txs.ConvertSubnetToL1Validator,
	_ ...commonopts.Option,
) (*txs.Tx, error) {
	w.conversions = append(w.conversions, conversionCall{
		subnetID:  subnetID,
		managerID: managerID,
		address:   append([]byte(nil), address...),
		values:    values,
	})
	return w.next("convert"), nil
}

func TestCreateRunsManagerBeforeMainAndNeverRegistersRPC(t *testing.T) {
	dir := t.TempDir()
	templatePath := filepath.Join(dir, "genesis-template.json")
	template := `{
		"config":{"chainId":99999},"alloc":{},"nonce":"0x0",
		"timestamp":"0x0","extraData":"0x00","gasLimit":"0x1",
		"difficulty":"0x0","mixHash":"0x0","coinbase":"0x0",
		"number":"0x0","gasUsed":"0x0","parentHash":"0x0"
	}`
	if err := os.WriteFile(templatePath, []byte(template), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Environment: config.Environment{
			Network:           "fuji",
			PChainAPI:         "https://example.invalid",
			FundingPrivateKey: strings.Repeat("1", 64),
			ManagerCommittee:  1,
		},
		Nodes: []config.Node{
			{Number: 1, Host: "v1", Role: config.RoleValidator},
			{Number: 2, Host: "v2", Role: config.RoleValidator},
			{Number: 3, Host: "v3", Role: config.RoleValidator},
			{Number: 4, Host: "v4", Role: config.RoleValidator},
			{Number: 5, Host: "rpc", Role: config.RoleRPC},
		},
	}
	wallet := &fakeWallet{}
	var walletConfigs []primary.WalletConfig
	factory := func(
		_ context.Context,
		_ string,
		_ keychain.Keychain,
		walletConfig primary.WalletConfig,
	) (pwallet.Wallet, error) {
		walletConfigs = append(walletConfigs, walletConfig)
		return wallet, nil
	}

	output := filepath.Join(dir, "deployment")
	result, err := create(context.Background(), cfg, output, templatePath, factory)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{"create-subnet", "create-chain", "convert", "create-subnet", "create-chain", "convert"}
	if !reflect.DeepEqual(wallet.events, wantEvents) {
		t.Fatalf("unexpected transaction order: got %v, want %v", wallet.events, wantEvents)
	}
	if len(walletConfigs) != 3 || len(walletConfigs[0].SubnetIDs) != 0 || len(walletConfigs[1].SubnetIDs) != 1 || len(walletConfigs[2].SubnetIDs) != 2 {
		t.Fatalf("unexpected wallet refreshes: %+v", walletConfigs)
	}
	if len(wallet.conversions) != 2 {
		t.Fatalf("expected two conversions, got %d", len(wallet.conversions))
	}
	manager := wallet.conversions[0]
	if manager.subnetID != result.State.ManagerSubnetID || manager.managerID != result.State.ManagerChainID || len(manager.values) != 1 {
		t.Fatalf("unexpected manager conversion: %+v", manager)
	}
	if manager.values[0].Weight != managerWeight || manager.values[0].Balance != initialBalance {
		t.Fatalf("unexpected manager validator: %+v", manager.values[0])
	}
	main := wallet.conversions[1]
	if main.subnetID != result.State.SubnetID || main.managerID != result.State.ManagerChainID || len(main.values) != 4 {
		t.Fatalf("unexpected main conversion: %+v", main)
	}
	highCount := 0
	lowCount := 0
	for _, validator := range main.values {
		if validator.Balance != initialBalance {
			t.Fatalf("unexpected initial balance: %d", validator.Balance)
		}
		switch validator.Weight {
		case highWeight:
			highCount++
		case lowWeight:
			lowCount++
		default:
			t.Fatalf("unexpected weight: %d", validator.Weight)
		}
	}
	if highCount != 3 || lowCount != 1 {
		t.Fatalf("unexpected weight split: high=%d low=%d", highCount, lowCount)
	}
	if _, err := os.Stat(filepath.Join(output, "nodes", "5", "signer.key")); !os.IsNotExist(err) {
		t.Fatalf("RPC signer key must not exist, got %v", err)
	}
	if result.State.ManagerConvertTxID == ids.Empty || result.State.ConvertTxID == ids.Empty {
		t.Fatalf("conversion transaction IDs not recorded: %+v", result.State)
	}
	if constants.SubnetEVMID == ids.Empty {
		t.Fatal("unexpected empty Subnet-EVM VM ID")
	}
	if _, err := create(context.Background(), cfg, output, templatePath, factory); err == nil {
		t.Fatal("create must refuse existing output")
	}
}

func TestRequiredFreshCreateBalanceIncludesAllRegistrationsAndFeeReserve(t *testing.T) {
	cfg := config.Config{
		Environment: config.Environment{ManagerCommittee: 4},
		Nodes: []config.Node{
			{Role: config.RoleValidator},
			{Role: config.RoleValidator},
			{Role: config.RoleValidator},
			{Role: config.RoleValidator},
			{Role: config.RoleRPC},
		},
	}
	want := uint64(9) * initialBalance
	if got := requiredFreshCreateBalance(cfg); got != want {
		t.Fatalf("unexpected required balance: got %d, want %d", got, want)
	}
}
