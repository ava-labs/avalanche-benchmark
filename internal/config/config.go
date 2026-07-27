package config

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
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

type Node struct {
	Number int
	Host   string
	Role   Role
	DC     string
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
	// PChainAPIToken is the optional rate-limit bypass token for the public
	// API. It is a secret and never belongs in a committed file or a build.
	PChainAPIToken string
}

type FleetEnvironment struct {
	Network    string
	SSHUser    string
	SSHKeyPath string
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
		"PCHAIN_API_TOKEN":    {},
		"FUNDING_PRIVATE_KEY": {},
		"SSH_USER":            {},
		"SSH_KEY_PATH":        {},
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
	return FleetEnvironment{
		Network:    networkEnvironment.Network,
		SSHUser:    sshUser,
		SSHKeyPath: sshKeyPath,
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
			"%s: PCHAIN_API must not carry a query string; the AvalancheGo client overwrites it. Use PCHAIN_API_TOKEN instead",
			path,
		)
	}
	return NetworkEnvironment{
		Network:        network,
		PChainAPI:      pChainAPI,
		PChainAPIToken: strings.TrimSpace(values["PCHAIN_API_TOKEN"]),
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
			return nil, fmt.Errorf("%s:%d: expected <node-number> host=<address> role=validator|rpc|pchain|archive|oracle-validator|oracle-rpc [dc=<tag>]", path, lineNumber)
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
			if key != "host" && key != "role" && key != "dc" {
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
		nodes = append(nodes, Node{Number: number, Host: host, Role: role, DC: values["dc"]})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read inventory %s: %w", path, err)
	}

	validatorCount := 0
	rpcCount := 0
	pchainCount := 0
	archiveCount := 0
	oracleValidatorCount := 0
	oracleRPCCount := 0
	for _, node := range nodes {
		switch node.Role {
		case RoleValidator:
			validatorCount++
		case RoleRPC:
			rpcCount++
		case RolePChain:
			pchainCount++
		case RoleArchive:
			archiveCount++
		case RoleOracleValidator:
			oracleValidatorCount++
		case RoleOracleRPC:
			oracleRPCCount++
		}
	}
	if validatorCount < 4 {
		return nil, fmt.Errorf("%s: expected at least 4 validators, found %d", path, validatorCount)
	}
	if rpcCount < 1 {
		return nil, fmt.Errorf("%s: expected at least 1 rpc node, found %d", path, rpcCount)
	}
	if pchainCount != 1 {
		return nil, fmt.Errorf("%s: expected exactly 1 P-chain node, found %d", path, pchainCount)
	}
	// A single archive cannot cross-check its own answers and leaves no
	// replica while it re-executes from genesis after a loss.
	if archiveCount == 1 {
		return nil, fmt.Errorf("%s: expected 0 or at least 2 archive nodes, found 1", path)
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
	return nodes, nil
}
