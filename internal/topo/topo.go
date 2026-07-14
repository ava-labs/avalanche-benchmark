// Package topo loads nodes.ini, the fleet inventory and the single source of
// truth for node names, hosts and roles. Every consumer (cmd/reconcile,
// cmd/l1, cmd/fuji-wallet, cmd/genstaking; the shell scripts go through
// `fleet endpoints`) keys off the node NAME, which is also the staking key
// directory (staking/l1/<name>), the manifest key in staking/node-ids.env and
// the node's data root on its box (data/<name>).
//
// Format: Ansible-host-line syntax, no sections. One node per line:
//
//	<name> host=<ip> role=validator|rpc [dc=<tag>]
//
// Comments (#) and blank lines are allowed. Rules:
//   - name: the primary key everywhere; letters, digits, '_', '-', '.' only.
//   - host= required. role= required: validator (a registered L1 validator;
//     a "spare" is just a validator at low on-chain weight) or rpc (tracks the
//     chain and serves ingress; never registered, never wears a BLS signer key).
//   - dc= optional, display-only grouping tag (`fleet status` groups by it,
//     fleet verbs accept `dc=<tag>` selectors). Nothing functional depends on it.
//
// Weights are NOT inventory: `l1 create` registers every validator at a
// constant initial weight and the real distribution is applied afterwards via
// `l1 apply` (scenarios/00_healthy.sh); the on-chain weight is the sole truth.
//
// Ports are positional per host: the k-th node on a host (file order) serves
// HTTP on 9650+2k and staking p2p on 9651+2k, so a one-node-per-host fleet is
// uniformly 9650/9651. Reordering nodes that share a host shifts the later
// nodes' ports: redeploy that host's nodes after such an edit.
package topo

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	RoleValidator = "validator"
	RoleRPC       = "rpc"

	// File is the canonical inventory filename, in the repo root next to .env.
	File = "nodes.ini"

	// baseHTTPPort/portStride set the positional port assignment: the k-th
	// node on a host gets HTTP baseHTTPPort+stride*k and staking +1.
	baseHTTPPort = 9650
	portStride   = 2
)

// Node is one line of nodes.ini.
type Node struct {
	Name string
	Host string
	Role string // RoleValidator or RoleRPC
	Port int    // HTTP/RPC port, assigned positionally per host
	DC   string // display-only grouping tag
}

func (n Node) IsValidator() bool { return n.Role == RoleValidator }

// StakingPort is the node's p2p port, always its HTTP port + 1.
func (n Node) StakingPort() int { return n.Port + 1 }

// Validators filters the inventory to the role=validator nodes, in file order.
func Validators(nodes []Node) []Node {
	var out []Node
	for _, n := range nodes {
		if n.IsValidator() {
			out = append(out, n)
		}
	}
	return out
}

// Load reads and parses the inventory file.
func Load(path string) ([]Node, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read fleet inventory: %w (nodes.ini lists every node: `<name> host=<ip> role=validator|rpc ...`; see the shipped nodes.ini)", err)
	}
	nodes, err := Parse(string(data))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return nodes, nil
}

// LoadNear finds nodes.ini the way the standalone tools find .env: next to the
// binary's parent dir (the bin/.. kit layout), else the working directory.
func LoadNear() ([]Node, error) {
	if exe, err := os.Executable(); err == nil {
		p := filepath.Join(filepath.Dir(exe), "..", File)
		if _, err := os.Stat(p); err == nil {
			return Load(p)
		}
	}
	return Load(File)
}

// Parse parses nodes.ini content: comments and blank lines skipped, first
// field the node name, the rest key=value pairs. Duplicate names, unknown
// keys and bad roles are all errors.
func Parse(data string) ([]Node, error) {
	var nodes []Node
	lineOf := map[string]int{} // node name -> 1-based line, for dup reporting
	perHost := map[string]int{}

	for lineNo, raw := range strings.Split(data, "\n") {
		line := raw
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		n := Node{Name: fields[0]}
		errf := func(format string, args ...any) error {
			return fmt.Errorf("line %d (%s): "+format, append([]any{lineNo + 1, n.Name}, args...)...)
		}
		if !validName(n.Name) {
			return nil, errf("bad node name (letters, digits, '_', '-', '.' only)")
		}
		if prev, dup := lineOf[n.Name]; dup {
			return nil, errf("duplicate node name (already defined on line %d)", prev)
		}
		lineOf[n.Name] = lineNo + 1

		for _, f := range fields[1:] {
			k, v, ok := strings.Cut(f, "=")
			if !ok || v == "" {
				return nil, errf("bad field %q (want key=value)", f)
			}
			switch k {
			case "host":
				n.Host = v
			case "role":
				n.Role = v
			case "dc":
				n.DC = v
			default:
				return nil, errf("unknown key %q (want host, role or dc; weights are on-chain only, move them with l1 apply)", k)
			}
		}
		if n.Host == "" {
			return nil, errf("host= is required")
		}
		switch n.Role {
		case RoleValidator, RoleRPC:
		case "":
			return nil, errf("role= is required (validator or rpc)")
		default:
			return nil, errf("bad role %q (want validator or rpc; a spare is just a validator at low weight)", n.Role)
		}
		n.Port = baseHTTPPort + portStride*perHost[n.Host]
		perHost[n.Host]++
		nodes = append(nodes, n)
	}
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes defined")
	}
	return nodes, nil
}

func validName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return s != ""
}
