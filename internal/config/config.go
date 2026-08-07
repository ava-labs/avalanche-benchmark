package config

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

type Role string

const (
	RoleValidator       Role = "validator"
	RoleRPC             Role = "rpc"
	RolePChain          Role = "pchain"
	RoleArchive         Role = "archive"
	RoleOracleValidator Role = "oracle-validator"
	RoleOracleRPC       Role = "oracle-rpc"
)

// MainChain is the default chain of every L1 role and the one whose state
// keeps the bare network.env keys (CHAIN_ID, SUBNET_ID, CONVERT_TX_ID).
const MainChain = "main"

// OracleChain is the chain the legacy oracle roles pin. The name is reserved:
// the oracle L1's application behavior (Warp receiver, relay) keys on the
// oracle roles, so a plain validator cannot join that chain.
const OracleChain = "oracle"

// chainNamePattern bounds chain names so they can name directories
// (chains/<name>/) and network.env keys (CHAIN_<NAME>_ID) without escaping.
var chainNamePattern = regexp.MustCompile(`^[a-z0-9-]{1,20}$`)

type Node struct {
	Number int
	Host   string
	Role   Role
	DC     string
	// Chain is the L1 this node serves, already resolved: "main" when the
	// inventory omits chain=, "oracle" for the oracle roles, and empty for
	// the P-chain node, which serves every chain and belongs to none.
	Chain string
	// Weight is the explicit initial stake weight from weight=. Zero means
	// the tag is absent and the default weight ladder applies. The tag is
	// valid only on the validator and oracle-validator roles, and a chain
	// sets it on all of its validators or on none.
	Weight uint64
}

// Chains returns the unique chain names the inventory declares, main first
// when present and the rest in name order. The P-chain node declares no
// chain and is skipped.
func Chains(nodes []Node) []string {
	seen := make(map[string]struct{}, 4)
	var names []string
	for _, node := range nodes {
		if node.Chain == "" {
			continue
		}
		if _, exists := seen[node.Chain]; exists {
			continue
		}
		seen[node.Chain] = struct{}{}
		names = append(names, node.Chain)
	}
	SortChains(names)
	return names
}

// SortChains orders chain names for display and iteration: main first when
// present, the rest by name.
func SortChains(names []string) {
	sort.Slice(names, func(i, j int) bool {
		if names[i] == MainChain || names[j] == MainChain {
			return names[i] == MainChain
		}
		return names[i] < names[j]
	})
}

// ValidChainName reports whether a name fits the chain naming rule.
func ValidChainName(name string) bool {
	return chainNamePattern.MatchString(name)
}

// EffectiveChain resolves the chain a role serves when the recorded chain
// may be empty: oracle roles always serve the oracle chain, the P-chain
// node serves none, and everything else defaults to main.
func EffectiveChain(role Role, chain string) string {
	if chain != "" {
		return chain
	}
	switch role {
	case RoleOracleValidator, RoleOracleRPC:
		return OracleChain
	case RolePChain:
		return ""
	default:
		return MainChain
	}
}

type Environment struct {
	Network           string
	PChainAPI         string
	FundingPrivateKey string
	SSHUser           string
	SSHKeyPath        string
}

type NetworkEnvironment struct {
	Network   string
	PChainAPI string
}

type FleetEnvironment struct {
	Network    string
	SSHUser    string
	SSHKeyPath string
	// RemoteDir is the root of the user-level install: every fleet file
	// lives under this directory on the machines, nothing needs root, and
	// nodes run as plain processes. Empty means the default root,
	// /home/<SSH_USER>/avalanche-benchmark. The user-level install is the
	// DEFAULT; nothing in it assumes sudo, systemd, or a Linux group named
	// after the user, so it runs on locked-down RHEL hosts as-is.
	RemoteDir string
	// RemoteDataDir overrides where chain databases and logs live, so data
	// can sit on a faster disk than the install. Empty means the install
	// root's data/ subdirectory.
	RemoteDataDir string
	// SystemInstall selects the legacy root install: /opt, /etc, /var/lib,
	// systemd units, sudo everywhere, restart on failure and on boot. It
	// exists for hosts where boot persistence matters more than running
	// without root, and it cannot be combined with REMOTE_DIR.
	SystemInstall bool
}

type Config struct {
	Environment Environment
	Nodes       []Node
}

func Load(root string) (Config, error) {
	return LoadFiles(
		filepath.Join(root, ".env"),
		filepath.Join(root, "nodes.ini"),
	)
}

func LoadFiles(envPath, nodesPath string) (Config, error) {
	env, err := LoadEnvironment(envPath)
	if err != nil {
		return Config{}, err
	}
	nodes, err := LoadNodes(nodesPath)
	if err != nil {
		return Config{}, err
	}
	return Config{Environment: env, Nodes: nodes}, nil
}

func LoadEnvironment(path string) (Environment, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		return Environment{}, fmt.Errorf("read required configuration %s: %w", path, err)
	}

	networkEnvironment, err := parseNetworkEnvironment(path, values)
	if err != nil {
		return Environment{}, err
	}

	if err := validateEnvironmentFields(path, values); err != nil {
		return Environment{}, err
	}

	required := func(key string) (string, error) {
		value := strings.TrimSpace(values[key])
		if value == "" {
			return "", fmt.Errorf("%s: required field %s is not provided", path, key)
		}
		return value, nil
	}

	fundingPrivateKey, err := required("FUNDING_PRIVATE_KEY")
	if err != nil {
		return Environment{}, err
	}
	if strings.HasPrefix(fundingPrivateKey, "0x") {
		return Environment{}, fmt.Errorf("%s: FUNDING_PRIVATE_KEY must not have a 0x prefix", path)
	}
	keyBytes, err := hex.DecodeString(fundingPrivateKey)
	if err != nil || len(keyBytes) != 32 {
		return Environment{}, fmt.Errorf("%s: FUNDING_PRIVATE_KEY must be exactly 64 hex characters", path)
	}
	allZero := true
	for _, b := range keyBytes {
		allZero = allZero && b == 0
	}
	if allZero {
		return Environment{}, fmt.Errorf("%s: FUNDING_PRIVATE_KEY must not be zero", path)
	}

	return Environment{
		Network:           networkEnvironment.Network,
		PChainAPI:         networkEnvironment.PChainAPI,
		FundingPrivateKey: fundingPrivateKey,
		SSHUser:           strings.TrimSpace(values["SSH_USER"]),
		SSHKeyPath:        strings.TrimSpace(values["SSH_KEY_PATH"]),
	}, nil
}

func validateEnvironmentFields(path string, values map[string]string) error {
	allowed := map[string]struct{}{
		"NETWORK":             {},
		"PCHAIN_API":          {},
		"FUNDING_PRIVATE_KEY": {},
		"SSH_USER":            {},
		"SSH_KEY_PATH":        {},
		"REMOTE_DIR":          {},
		"REMOTE_DATA_DIR":     {},
		"SYSTEM_INSTALL":      {},
	}
	var unknown []string
	for key := range values {
		if _, ok := allowed[key]; !ok {
			unknown = append(unknown, key)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return fmt.Errorf("%s: unknown field(s): %s", path, strings.Join(unknown, ", "))
	}
	return nil
}

// LoadFleetEnvironment reads only deployment settings. Fleet deployment must
// not require the funding private key used by l1.
func LoadFleetEnvironment(path string) (FleetEnvironment, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		return FleetEnvironment{}, fmt.Errorf("read required configuration %s: %w", path, err)
	}
	if err := validateEnvironmentFields(path, values); err != nil {
		return FleetEnvironment{}, err
	}
	networkEnvironment, err := parseNetworkEnvironment(path, values)
	if err != nil {
		return FleetEnvironment{}, err
	}
	required := func(key string) (string, error) {
		value := strings.TrimSpace(values[key])
		if value == "" {
			return "", fmt.Errorf("%s: required field %s is not provided", path, key)
		}
		return value, nil
	}
	sshUser, err := required("SSH_USER")
	if err != nil {
		return FleetEnvironment{}, err
	}
	sshKeyPath, err := required("SSH_KEY_PATH")
	if err != nil {
		return FleetEnvironment{}, err
	}
	if info, err := os.Stat(sshKeyPath); err != nil {
		return FleetEnvironment{}, fmt.Errorf("%s: SSH_KEY_PATH %s is unavailable: %w", path, sshKeyPath, err)
	} else if info.IsDir() {
		return FleetEnvironment{}, fmt.Errorf("%s: SSH_KEY_PATH %s is a directory", path, sshKeyPath)
	}
	remoteDir := strings.TrimSpace(values["REMOTE_DIR"])
	remoteDataDir := strings.TrimSpace(values["REMOTE_DATA_DIR"])
	systemInstall := false
	switch strings.TrimSpace(values["SYSTEM_INSTALL"]) {
	case "", "false":
	case "true":
		systemInstall = true
	default:
		return FleetEnvironment{}, fmt.Errorf("%s: SYSTEM_INSTALL must be true or false, got %q", path, values["SYSTEM_INSTALL"])
	}
	if systemInstall && remoteDir != "" {
		return FleetEnvironment{}, fmt.Errorf("%s: SYSTEM_INSTALL=true cannot be combined with REMOTE_DIR; a system install has fixed paths", path)
	}
	if systemInstall && remoteDataDir != "" {
		return FleetEnvironment{}, fmt.Errorf("%s: SYSTEM_INSTALL=true cannot be combined with REMOTE_DATA_DIR; a system install has fixed paths", path)
	}
	return FleetEnvironment{
		Network:       networkEnvironment.Network,
		SSHUser:       sshUser,
		SSHKeyPath:    sshKeyPath,
		RemoteDir:     remoteDir,
		RemoteDataDir: remoteDataDir,
		SystemInstall: systemInstall,
	}, nil
}

// LoadNetworkEnvironment reads only the fields needed by fleet operations.
// Fleet lifecycle must not require the funding private key used by l1.
func LoadNetworkEnvironment(path string) (NetworkEnvironment, error) {
	values, err := godotenv.Read(path)
	if err != nil {
		return NetworkEnvironment{}, fmt.Errorf("read required configuration %s: %w", path, err)
	}
	return parseNetworkEnvironment(path, values)
}

func parseNetworkEnvironment(path string, values map[string]string) (NetworkEnvironment, error) {
	network := strings.TrimSpace(values["NETWORK"])
	if network == "" {
		return NetworkEnvironment{}, fmt.Errorf("%s: required field NETWORK is not provided", path)
	}
	if network != "fuji" && network != "mainnet" {
		return NetworkEnvironment{}, fmt.Errorf("%s: NETWORK must be fuji or mainnet, got %q", path, network)
	}

	pChainAPI := strings.TrimSpace(values["PCHAIN_API"])
	if pChainAPI == "" {
		return NetworkEnvironment{}, fmt.Errorf("%s: required field PCHAIN_API is not provided", path)
	}
	parsedAPI, err := url.Parse(pChainAPI)
	if err != nil || parsedAPI.Host == "" || (parsedAPI.Scheme != "http" && parsedAPI.Scheme != "https") {
		return NetworkEnvironment{}, fmt.Errorf("%s: PCHAIN_API must be an explicit http or https URL, got %q", path, pChainAPI)
	}
	if parsedAPI.RawQuery != "" {
		return NetworkEnvironment{}, fmt.Errorf(
			"%s: PCHAIN_API must not carry a query string; the AvalancheGo client overwrites it",
			path,
		)
	}
	return NetworkEnvironment{
		Network:   network,
		PChainAPI: pChainAPI,
	}, nil
}

func LoadNodes(path string) ([]Node, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read required inventory %s: %w", path, err)
	}
	defer f.Close()

	var nodes []Node
	seenNumbers := make(map[int]int)
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := scanner.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 3 {
			return nil, fmt.Errorf("%s:%d: expected <node-number> host=<address> role=validator|rpc|pchain|archive|oracle-validator|oracle-rpc [chain=<name>] [weight=<n>] [dc=<tag>]", path, lineNumber)
		}

		number, err := strconv.Atoi(fields[0])
		if err != nil || number <= 0 {
			return nil, fmt.Errorf("%s:%d: node number must be a positive integer, got %q", path, lineNumber, fields[0])
		}
		if previousLine, ok := seenNumbers[number]; ok {
			return nil, fmt.Errorf("%s:%d: duplicate node number %d, first declared on line %d", path, lineNumber, number, previousLine)
		}
		seenNumbers[number] = lineNumber

		values := make(map[string]string)
		for _, field := range fields[1:] {
			key, value, ok := strings.Cut(field, "=")
			if !ok || key == "" || value == "" {
				return nil, fmt.Errorf("%s:%d: expected key=value, got %q", path, lineNumber, field)
			}
			if key != "host" && key != "role" && key != "dc" && key != "chain" && key != "weight" {
				return nil, fmt.Errorf("%s:%d: unknown node field %q", path, lineNumber, key)
			}
			if _, exists := values[key]; exists {
				return nil, fmt.Errorf("%s:%d: duplicate node field %q", path, lineNumber, key)
			}
			values[key] = value
		}

		host := values["host"]
		if host == "" {
			return nil, fmt.Errorf("%s:%d: required node field host is not provided", path, lineNumber)
		}
		role := Role(values["role"])
		switch role {
		case RoleValidator, RoleRPC, RolePChain, RoleArchive, RoleOracleValidator, RoleOracleRPC:
		default:
			return nil, fmt.Errorf("%s:%d: role must be validator, rpc, pchain, archive, oracle-validator, or oracle-rpc, got %q", path, lineNumber, values["role"])
		}
		chain := values["chain"]
		if chain != "" && !chainNamePattern.MatchString(chain) {
			return nil, fmt.Errorf("%s:%d: chain must be 1 to 20 characters of lowercase letters, digits, and hyphens, got %q", path, lineNumber, chain)
		}
		switch role {
		case RolePChain:
			// The P-chain node serves every chain: it is the bootstrap
			// rendezvous for all of them and tracks none.
			if chain != "" {
				return nil, fmt.Errorf("%s:%d: the P-chain node serves every chain; chain= is not valid with role=pchain", path, lineNumber)
			}
		case RoleOracleValidator, RoleOracleRPC:
			if chain != "" && chain != OracleChain {
				return nil, fmt.Errorf("%s:%d: role %s always serves chain %q, got chain=%q", path, lineNumber, role, OracleChain, chain)
			}
			chain = OracleChain
		default:
			if chain == "" {
				chain = MainChain
			}
			if chain == OracleChain {
				return nil, fmt.Errorf("%s:%d: chain name %q is reserved for the oracle-validator and oracle-rpc roles", path, lineNumber, OracleChain)
			}
			if chain == "management" {
				return nil, fmt.Errorf("%s:%d: chain name %q is reserved for the management chain", path, lineNumber, chain)
			}
			if chain == "default" {
				return nil, fmt.Errorf("%s:%d: chain name %q is reserved for the shared defaults at chains/default/", path, lineNumber, chain)
			}
		}
		var weight uint64
		if raw, present := values["weight"]; present {
			// An explicit weight replaces the default ladder for this node.
			// The role check keeps the tag on stake-carrying roles only.
			if role != RoleValidator && role != RoleOracleValidator {
				return nil, fmt.Errorf("%s:%d: weight= is valid only with role=validator or role=oracle-validator, got role=%s", path, lineNumber, role)
			}
			parsed, err := strconv.ParseUint(raw, 10, 64)
			if err != nil || parsed == 0 {
				return nil, fmt.Errorf("%s:%d: weight must be an integer of at least 1, got %q", path, lineNumber, raw)
			}
			weight = parsed
		}
		nodes = append(nodes, Node{Number: number, Host: host, Role: role, DC: values["dc"], Chain: chain, Weight: weight})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read inventory %s: %w", path, err)
	}

	pchainCount := 0
	oracleValidatorCount := 0
	oracleRPCCount := 0
	type chainShape struct {
		validators int
		rpcs       int
		archives   int
	}
	shapes := make(map[string]*chainShape, 2)
	shapeOf := func(chain string) *chainShape {
		shape, exists := shapes[chain]
		if !exists {
			shape = &chainShape{}
			shapes[chain] = shape
		}
		return shape
	}
	for _, node := range nodes {
		switch node.Role {
		case RoleValidator:
			shapeOf(node.Chain).validators++
		case RoleRPC:
			shapeOf(node.Chain).rpcs++
		case RolePChain:
			pchainCount++
		case RoleArchive:
			shapeOf(node.Chain).archives++
		case RoleOracleValidator:
			oracleValidatorCount++
		case RoleOracleRPC:
			oracleRPCCount++
		}
	}
	// Structural rules stay hard errors: deploy cannot work without them.
	// Shape opinions (validator count, RPC count, archive redundancy) are
	// warnings per chain, so an operator can experiment with any topology and
	// still hears why the recommended shape is what it is.
	if pchainCount != 1 {
		return nil, fmt.Errorf("%s: expected exactly 1 P-chain node, found %d", path, pchainCount)
	}
	for _, chain := range Chains(nodes) {
		if chain == OracleChain {
			continue
		}
		shape := shapeOf(chain)
		// A chain with no validator cannot convert to an L1, so declaring one
		// through rpc or archive lines alone is structurally impossible.
		if shape.validators == 0 {
			return nil, fmt.Errorf("%s: chain %q declares no validator; a chain needs at least 1 role=validator node", path, chain)
		}
		if shape.validators < 4 {
			fmt.Fprintf(os.Stderr, "warning: %s declares %d validator(s) on chain %q; the tested failover shape uses 4 or more (3 heavy + spares), and fewer validators tolerate less loss\n", path, shape.validators, chain)
		}
		if shape.rpcs < 1 {
			fmt.Fprintf(os.Stderr, "warning: %s declares no rpc node on chain %q; bombard and transaction ingress need role=rpc, and serving transactions on a validator slows its block production\n", path, chain)
		}
		// A single archive cannot cross-check its own answers and leaves no
		// replica while it re-executes from genesis after a loss.
		if shape.archives == 1 {
			fmt.Fprintf(os.Stderr, "warning: %s declares a single archive node on chain %q; it cannot cross-check its answers and re-executes from genesis alone after a loss\n", path, chain)
		}
	}
	// The oracle L1 is opt-in: no oracle nodes means no oracle chain. When it
	// exists, its feed ingress must stay off its validators, same as main.
	if oracleValidatorCount > 0 && oracleRPCCount < 1 {
		return nil, fmt.Errorf("%s: oracle validators require at least 1 oracle-rpc node, found 0", path)
	}
	if oracleRPCCount > 0 && oracleValidatorCount == 0 {
		return nil, fmt.Errorf("%s: oracle-rpc nodes require at least 1 oracle-validator, found 0", path)
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Number < nodes[j].Number })
	if err := validateWeightConsistency(path, nodes); err != nil {
		return nil, err
	}
	return nodes, nil
}

// validateWeightConsistency enforces the all-or-none rule for explicit
// weights: a chain sets weight= on every one of its validators or on none.
// A mix would make the effective weights depend on line order, so it is a
// hard error that names the chain and the node numbers on each side.
func validateWeightConsistency(path string, nodes []Node) error {
	withWeight := make(map[string][]int)
	withoutWeight := make(map[string][]int)
	for _, node := range nodes {
		if node.Role != RoleValidator && node.Role != RoleOracleValidator {
			continue
		}
		if node.Weight > 0 {
			withWeight[node.Chain] = append(withWeight[node.Chain], node.Number)
		} else {
			withoutWeight[node.Chain] = append(withoutWeight[node.Chain], node.Number)
		}
	}
	for _, chain := range Chains(nodes) {
		explicit := withWeight[chain]
		defaulted := withoutWeight[chain]
		if len(explicit) == 0 || len(defaulted) == 0 {
			continue
		}
		return fmt.Errorf(
			"%s: chain %q mixes explicit and default validator weights: weight= is set on node(s) %s and not set on node(s) %s; set weight= on every validator of the chain or on none",
			path, chain, joinNumbers(explicit), joinNumbers(defaulted),
		)
	}
	return nil
}

func joinNumbers(numbers []int) string {
	parts := make([]string, len(numbers))
	for i, number := range numbers {
		parts[i] = strconv.Itoa(number)
	}
	return strings.Join(parts, ", ")
}
