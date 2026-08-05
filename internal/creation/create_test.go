package creation

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanche-benchmark/remote/internal/identity"
	"github.com/ava-labs/avalanchego/ids"
	"github.com/ava-labs/avalanchego/utils/constants"
	"github.com/ava-labs/avalanchego/utils/crypto/keychain"
	"github.com/ava-labs/avalanchego/vms/platformvm/txs"
	"github.com/ava-labs/avalanchego/vms/secp256k1fx"
	pwallet "github.com/ava-labs/avalanchego/wallet/chain/p/wallet"
	"github.com/ava-labs/avalanchego/wallet/subnet/primary"
	commonopts "github.com/ava-labs/avalanchego/wallet/subnet/primary/common"
	ethcommon "github.com/ava-labs/libevm/common"
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
	// Without oracle nodes the oracle template must never be read.
	oracleTemplatePath := filepath.Join(dir, "oracle-genesis-template.json")
	cfg := config.Config{
		Environment: config.Environment{
			Network:           "fuji",
			PChainAPI:         "https://example.invalid",
			FundingPrivateKey: strings.Repeat("1", 64),
		},
		Nodes: []config.Node{
			{Number: 1, Host: "v1", Role: config.RoleValidator},
			{Number: 2, Host: "v2", Role: config.RoleValidator},
			{Number: 3, Host: "v3", Role: config.RoleValidator},
			{Number: 4, Host: "v4", Role: config.RoleValidator},
			{Number: 5, Host: "rpc", Role: config.RoleRPC},
			{Number: 6, Host: "pchain", Role: config.RolePChain},
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
	if err := os.Mkdir(output, 0o700); err != nil {
		t.Fatal(err)
	}
	privateOutput := filepath.Join(dir, "private")
	if err := os.Mkdir(privateOutput, 0o700); err != nil {
		t.Fatal(err)
	}
	generated, err := identity.Generate(privateOutput, cfg.Nodes, 1)
	if err != nil {
		t.Fatal(err)
	}
	feeder := ethcommon.HexToAddress("0xAbcDef0123456789abCDef0123456789ABcdEF01")
	public := NewPublic(generated, ethcommon.HexToAddress("0x1234567890123456789012345678901234567890"), feeder)
	if _, err := SavePublic(filepath.Join(output, "public.json"), public); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadPublic(filepath.Join(output, "public.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := create(context.Background(), cfg.Environment, output, templatePath, oracleTemplatePath, loaded, factory)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{"create-subnet", "create-chain", "convert", "create-subnet", "create-chain", "convert"}
	if !reflect.DeepEqual(wallet.events, wantEvents) {
		t.Fatalf("unexpected transaction order: got %v, want %v", wallet.events, wantEvents)
	}
	// One rebuild per issuance: the wallet caches its UTXO set, so reusing one
	// across issuances is what produced "failed to read consumed UTXO" live.
	// Each rebuild must carry every subnet known at that point.
	wantSubnetCounts := []int{0, 1, 1, 1, 2, 2}
	if len(walletConfigs) != len(wantSubnetCounts) {
		t.Fatalf("expected %d wallet rebuilds, got %d: %+v", len(wantSubnetCounts), len(walletConfigs), walletConfigs)
	}
	for i, want := range wantSubnetCounts {
		if got := len(walletConfigs[i].SubnetIDs); got != want {
			t.Fatalf("wallet rebuild %d tracked %d subnet(s), want %d: %+v", i, got, want, walletConfigs)
		}
	}
	if len(wallet.conversions) != 2 {
		t.Fatalf("expected two conversions, got %d", len(wallet.conversions))
	}
	manager := wallet.conversions[0]
	if manager.subnetID != result.State.ManagerSubnetID || manager.managerID != result.State.ManagerChainID || len(manager.values) != 1 {
		t.Fatalf("unexpected manager conversion: %+v", manager)
	}
	if manager.values[0].Weight != ManagerWeight || manager.values[0].Balance != initialBalance {
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
		case HighWeight:
			highCount++
		case LowWeight:
			lowCount++
		default:
			t.Fatalf("unexpected weight: %d", validator.Weight)
		}
	}
	if highCount != 3 || lowCount != 1 {
		t.Fatalf("unexpected weight split: high=%d low=%d", highCount, lowCount)
	}
	mainGenesis, err := os.ReadFile(filepath.Join(output, "genesis.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mainDocument genesisDocument
	if err := json.Unmarshal(mainGenesis, &mainDocument); err != nil {
		t.Fatal(err)
	}
	assertGenesisIsBaseLayerOnly(t, mainDocument)
	if mainDocument.Alloc[allocKey(feeder)].Balance != genesisBalance {
		t.Fatal("feeder not funded on the main chain")
	}
	if _, err := os.Stat(filepath.Join(output, "identities")); !os.IsNotExist(err) {
		t.Fatalf("creation output must not need private identities, got %v", err)
	}
	if result.State.ManagerConvertTxID == ids.Empty || result.State.ConvertTxID == ids.Empty {
		t.Fatalf("conversion transaction IDs not recorded: %+v", result.State)
	}
	if constants.SubnetEVMID == ids.Empty {
		t.Fatal("unexpected empty Subnet-EVM VM ID")
	}
	if _, err := create(context.Background(), cfg.Environment, output, templatePath, oracleTemplatePath, loaded, factory); err == nil {
		t.Fatal("create must refuse existing genesis")
	}
}

func TestCreateWithOracleRunsManagerOracleMain(t *testing.T) {
	dir := t.TempDir()
	template := `{
		"config":{"chainId":99999},"alloc":{},"nonce":"0x0",
		"timestamp":"0x0","extraData":"0x00","gasLimit":"0x1",
		"difficulty":"0x0","mixHash":"0x0","coinbase":"0x0",
		"number":"0x0","gasUsed":"0x0","parentHash":"0x0"
	}`
	templatePath := filepath.Join(dir, "genesis-template.json")
	if err := os.WriteFile(templatePath, []byte(template), 0o600); err != nil {
		t.Fatal(err)
	}
	oracleTemplate := strings.Replace(template, `"config":{"chainId":99999}`, `"config":{"chainId":99998,"feeManagerConfig":{"blockTimestamp":0}}`, 1)
	oracleTemplatePath := filepath.Join(dir, "oracle-genesis-template.json")
	if err := os.WriteFile(oracleTemplatePath, []byte(oracleTemplate), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{
		Environment: config.Environment{
			Network:           "fuji",
			PChainAPI:         "https://example.invalid",
			FundingPrivateKey: strings.Repeat("1", 64),
		},
		Nodes: []config.Node{
			{Number: 1, Host: "v1", Role: config.RoleValidator},
			{Number: 2, Host: "v2", Role: config.RoleValidator},
			{Number: 3, Host: "v3", Role: config.RoleValidator},
			{Number: 4, Host: "v4", Role: config.RoleValidator},
			{Number: 5, Host: "rpc", Role: config.RoleRPC},
			{Number: 6, Host: "pchain", Role: config.RolePChain},
			{Number: 7, Host: "o1", Role: config.RoleOracleValidator},
			{Number: 8, Host: "o2", Role: config.RoleOracleValidator},
			{Number: 9, Host: "rpc", Role: config.RoleOracleRPC},
		},
	}
	wallet := &fakeWallet{}
	factory := func(
		_ context.Context,
		_ string,
		_ keychain.Keychain,
		_ primary.WalletConfig,
	) (pwallet.Wallet, error) {
		return wallet, nil
	}

	output := filepath.Join(dir, "deployment")
	if err := os.Mkdir(output, 0o700); err != nil {
		t.Fatal(err)
	}
	privateOutput := filepath.Join(dir, "private")
	if err := os.Mkdir(privateOutput, 0o700); err != nil {
		t.Fatal(err)
	}
	generated, err := identity.Generate(privateOutput, cfg.Nodes, 1)
	if err != nil {
		t.Fatal(err)
	}
	feeder := ethcommon.HexToAddress("0xAbcDef0123456789abCDef0123456789ABcdEF01")
	public := NewPublic(generated, ethcommon.HexToAddress("0x1234567890123456789012345678901234567890"), feeder)
	if _, err := SavePublic(filepath.Join(output, "public.json"), public); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := LoadPublic(filepath.Join(output, "public.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := create(context.Background(), cfg.Environment, output, templatePath, oracleTemplatePath, loaded, factory)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := []string{
		"create-subnet", "create-chain", "convert",
		"create-subnet", "create-chain", "convert",
		"create-subnet", "create-chain", "convert",
	}
	if !reflect.DeepEqual(wallet.events, wantEvents) {
		t.Fatalf("unexpected transaction order: got %v, want %v", wallet.events, wantEvents)
	}
	if len(wallet.conversions) != 3 {
		t.Fatalf("expected three conversions, got %d", len(wallet.conversions))
	}
	oracle := wallet.conversions[1]
	if oracle.subnetID != result.State.OracleSubnetID || oracle.managerID != result.State.ManagerChainID || len(oracle.values) != 2 {
		t.Fatalf("unexpected oracle conversion: %+v", oracle)
	}
	for _, validator := range oracle.values {
		if validator.Weight != OracleWeight || validator.Balance != initialBalance {
			t.Fatalf("unexpected oracle validator: %+v", validator)
		}
	}
	if result.State.OracleConvertTxID == ids.Empty {
		t.Fatalf("oracle conversion transaction ID not recorded: %+v", result.State)
	}

	oracleGenesis, err := os.ReadFile(filepath.Join(output, "genesis-oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var oracleDocument genesisDocument
	if err := json.Unmarshal(oracleGenesis, &oracleDocument); err != nil {
		t.Fatal(err)
	}
	aggregator := oracleDocument.Alloc[allocKey(AggregatorAddress)]
	if aggregator.Code == "" || aggregator.Storage[ethcommon.Hash{}.Hex()] != ethcommon.BytesToHash(feeder.Bytes()).Hex() {
		t.Fatalf("aggregator not baked into oracle genesis: %+v", aggregator)
	}
	if oracleDocument.Alloc[allocKey(feeder)].Balance != genesisBalance {
		t.Fatal("feeder not funded on the oracle chain")
	}

	mainGenesis, err := os.ReadFile(filepath.Join(output, "genesis.json"))
	if err != nil {
		t.Fatal(err)
	}
	var mainDocument genesisDocument
	if err := json.Unmarshal(mainGenesis, &mainDocument); err != nil {
		t.Fatal(err)
	}
	receiver := mainDocument.Alloc[allocKey(ReceiverAddress)]
	if receiver.Code == "" {
		t.Fatalf("receiver not baked into main genesis: %+v", receiver)
	}
	if receiver.Storage[ethcommon.Hash{}.Hex()] != ethcommon.Hash(result.State.OracleChainID).Hex() {
		t.Fatalf("receiver source chain must be the oracle chain: %+v", receiver.Storage)
	}
	if receiver.Storage[ethcommon.BigToHash(ethcommon.Big1).Hex()] != ethcommon.BytesToHash(AggregatorAddress.Bytes()).Hex() {
		t.Fatalf("receiver origin sender must be the aggregator: %+v", receiver.Storage)
	}
	if mainDocument.Alloc[allocKey(feeder)].Balance != genesisBalance {
		t.Fatal("feeder not funded on the main chain")
	}
	// App contracts install through the upgrade history in every shape; the
	// receiver above is the flagged exception, not the rule.
	assertGenesisIsBaseLayerOnly(t, mainDocument)
}

// assertGenesisIsBaseLayerOnly pins the policy: the main genesis carries
// funded accounts and network shape, never app contracts. Apps install
// through the upgrade history, so adding one never forces a chain
// re-creation (a frozen P-chain makes a re-creation a full unfreeze,
// re-create, re-freeze, redeploy cycle).
func assertGenesisIsBaseLayerOnly(t *testing.T, mainDocument genesisDocument) {
	t.Helper()
	for name, address := range map[string]ethcommon.Address{
		"price aggregator": PriceFeedAggregatorAddress,
		"price feed proxy": PriceFeedAddress,
	} {
		account := mainDocument.Alloc[allocKey(address)]
		if account.Code != "" || len(account.Storage) != 0 {
			t.Fatalf("%s baked into main genesis; app contracts install through the upgrade history: %+v", name, account)
		}
	}
}

func TestRequiredFreshCreateBalanceIncludesAllRegistrationsAndFeeReserve(t *testing.T) {
	public := Public{
		Nodes: []PublicNode{
			{Role: config.RoleValidator},
			{Role: config.RoleValidator},
			{Role: config.RoleValidator},
			{Role: config.RoleValidator},
			{Role: config.RoleRPC},
			{Role: config.RolePChain},
		},
		Managers: make([]PublicManager, 4),
	}
	want := uint64(9) * initialBalance
	if got := requiredFreshCreateBalance(public); got != want {
		t.Fatalf("unexpected required balance: got %d, want %d", got, want)
	}
}

func TestValidateManagerCommittee(t *testing.T) {
	for _, size := range []int{1, 4} {
		if err := ValidateManagerCommittee(size); err != nil {
			t.Errorf("size %d: %v", size, err)
		}
	}
	for _, size := range []int{0, 2, 3, 5} {
		if err := ValidateManagerCommittee(size); err == nil {
			t.Errorf("size %d accepted", size)
		}
	}
}

// flakyWallet fails the first n issuances, imitating a stale UTXO set.
type flakyWallet struct {
	fakeWallet
	failures int
}

func (w *flakyWallet) IssueCreateSubnetTx(owners *secp256k1fx.OutputOwners, opts ...commonopts.Option) (*txs.Tx, error) {
	if w.failures > 0 {
		w.failures--
		return nil, errors.New("failed to read consumed UTXO 2hMVvavZV9RUCzgGY63q8ZFCYDX4vVDr2txhxANbsXUiBTjYL4:0 due to: not found")
	}
	return w.fakeWallet.IssueCreateSubnetTx(owners, opts...)
}

func TestIssueTxRebuildsTheWalletAndRetries(t *testing.T) {
	previous := issueBackoff
	issueBackoff = 0
	t.Cleanup(func() { issueBackoff = previous })

	wallet := &flakyWallet{failures: 2}
	rebuilds := 0
	factory := func(_ context.Context, _ string, _ keychain.Keychain, _ primary.WalletConfig) (pwallet.Wallet, error) {
		rebuilds++
		return wallet, nil
	}
	state := State{}
	tx, err := issueTx(context.Background(), "main CreateSubnetTx", "https://example.invalid", nil, &state, factory,
		func(w pwallet.Wallet) (*txs.Tx, error) { return w.IssueCreateSubnetTx(nil) })
	if err != nil {
		t.Fatal(err)
	}
	if tx == nil {
		t.Fatal("expected a transaction after the retries succeeded")
	}
	// Two failures then a success, and a fresh wallet for every attempt.
	if rebuilds != 3 {
		t.Fatalf("wallet rebuilds = %d, want 3 (one per attempt)", rebuilds)
	}
}

func TestIssueTxGivesUpAndReportsTheLastError(t *testing.T) {
	previous := issueBackoff
	issueBackoff = 0
	t.Cleanup(func() { issueBackoff = previous })

	wallet := &flakyWallet{failures: issueAttempts}
	factory := func(_ context.Context, _ string, _ keychain.Keychain, _ primary.WalletConfig) (pwallet.Wallet, error) {
		return wallet, nil
	}
	state := State{}
	_, err := issueTx(context.Background(), "main CreateSubnetTx", "https://example.invalid", nil, &state, factory,
		func(w pwallet.Wallet) (*txs.Tx, error) { return w.IssueCreateSubnetTx(nil) })
	if err == nil {
		t.Fatal("expected exhausted retries to fail")
	}
	if !strings.Contains(err.Error(), "main CreateSubnetTx") || !strings.Contains(err.Error(), "consumed UTXO") {
		t.Fatalf("error must name the action and carry the last cause, got: %v", err)
	}
}
