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
	RoleValidator Role = "validator"
	RoleRPC       Role = "rpc"
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
	ManagerCommittee  int
	SSHUser           string
	SSHKeyPath        string
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

	allowed := map[string]struct{}{
		"NETWORK":             {},
		"PCHAIN_API":          {},
		"FUNDING_PRIVATE_KEY": {},
		"MANAGER_COMMITTEE":   {},
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
		return Environment{}, fmt.Errorf("%s: unknown field(s): %s", path, strings.Join(unknown, ", "))
	}

	required := func(key string) (string, error) {
		value := strings.TrimSpace(values[key])
		if value == "" {
			return "", fmt.Errorf("%s: required field %s is not provided", path, key)
		}
		return value, nil
	}

	network, err := required("NETWORK")
	if err != nil {
		return Environment{}, err
	}
	if network != "fuji" && network != "mainnet" {
		return Environment{}, fmt.Errorf("%s: NETWORK must be fuji or mainnet, got %q", path, network)
	}

	pChainAPI, err := required("PCHAIN_API")
	if err != nil {
		return Environment{}, err
	}
	parsedAPI, err := url.Parse(pChainAPI)
	if err != nil || parsedAPI.Host == "" || (parsedAPI.Scheme != "http" && parsedAPI.Scheme != "https") {
		return Environment{}, fmt.Errorf("%s: PCHAIN_API must be an explicit http or https URL, got %q", path, pChainAPI)
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

	committeeRaw, err := required("MANAGER_COMMITTEE")
	if err != nil {
		return Environment{}, err
	}
	managerCommittee, err := strconv.Atoi(committeeRaw)
	if err != nil || (managerCommittee != 1 && managerCommittee != 4) {
		return Environment{}, fmt.Errorf("%s: MANAGER_COMMITTEE must be 1 or 4, got %q", path, committeeRaw)
	}

	return Environment{
		Network:           network,
		PChainAPI:         pChainAPI,
		FundingPrivateKey: fundingPrivateKey,
		ManagerCommittee:  managerCommittee,
		SSHUser:           strings.TrimSpace(values["SSH_USER"]),
		SSHKeyPath:        strings.TrimSpace(values["SSH_KEY_PATH"]),
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
			return nil, fmt.Errorf("%s:%d: expected <node-number> host=<address> role=validator|rpc [dc=<tag>]", path, lineNumber)
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
		if role != RoleValidator && role != RoleRPC {
			return nil, fmt.Errorf("%s:%d: role must be validator or rpc, got %q", path, lineNumber, values["role"])
		}
		nodes = append(nodes, Node{Number: number, Host: host, Role: role, DC: values["dc"]})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read inventory %s: %w", path, err)
	}

	validatorCount := 0
	rpcCount := 0
	for _, node := range nodes {
		switch node.Role {
		case RoleValidator:
			validatorCount++
		case RoleRPC:
			rpcCount++
		}
	}
	if validatorCount < 4 {
		return nil, fmt.Errorf("%s: expected at least 4 validators, found %d", path, validatorCount)
	}
	if rpcCount < 1 {
		return nil, fmt.Errorf("%s: expected at least 1 rpc node, found %d", path, rpcCount)
	}

	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Number < nodes[j].Number })
	return nodes, nil
}
