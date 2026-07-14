// Package topo loads nodes.ini, the fleet inventory and the single source of
// truth for node names, hosts and roles. Every consumer (cmd/reconcile,
// cmd/l1, cmd/fuji-wallet, cmd/genstaking; the shell scripts go through
// `fleet endpoints`) keys off the node NAME, which is also the staking key
// directory (staking/l1/<name>), the manifest key in staking/node-ids.env and
// the node's data root on its box (data/<name>).
//
// Format: Ansible-host-line syntax, no sections. One node per line:
//
//	<name> host=<ip> role=validator|rpc [dc=<tag>] [weight=<w>]
//
// Comments (#) and blank lines are allowed. Rules:
//   - name: the primary key everywhere; letters, digits, '_', '-', '.' only.
//   - host= required. role= required: validator (a registered L1 validator;
//     a "spare" is just a validator at weight 1) or rpc (tracks the chain and
//     serves ingress; never registered, never wears a BLS signer key).
//   - dc= optional, display-only grouping tag (`fleet status` groups by it,
//     fleet verbs accept `dc=<tag>` selectors). Nothing functional depends on it.
//   - weight= optional, role=validator only: the CONVERSION weight `l1 create`
//     registers the node with (default 1). Read only by create; after creation
//     the on-chain weight is the sole truth and this tag is never consulted.
//
// Ports are positional per host: the k-th node on a host (file order) serves
// HTTP on 9652+2k and staking p2p on 9653+2k, so a one-node-per-host fleet is
// uniformly 9652/9653. Reordering nodes that share a host shifts the later
// nodes' ports: redeploy that host's nodes after such an edit.
package topo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	RoleValidator = "validator"
	RoleRPC       = "rpc"

	// File is the canonical inventory filename, in the repo root next to .env.
	File = "nodes.ini"

	// baseHTTPPort/portStride set the positional port assignment: the k-th
	// node on a host gets HTTP baseHTTPPort+stride*k and staking +1.
	baseHTTPPort = 9652
	portStride   = 2
)

// Node is one line of nodes.ini.
type Node struct {
	Name   string
	Host   string
	Role   string // RoleValidator or RoleRPC
	Port   int    // HTTP/RPC port, assigned positionally per host
	DC     string // display-only grouping tag
	Weight uint64 // conversion weight, read ONLY by `l1 create` (default 1)
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
// keys, bad roles and a weight on a non-validator are all errors.
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
		n := Node{Name: fields[0], Weight: 1}
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

		weightSet := false
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
			case "weight":
				w, err := strconv.ParseUint(v, 10, 64)
				if err != nil || w == 0 {
					return nil, errf("bad weight %q (want a positive integer)", v)
				}
				n.Weight = w
				weightSet = true
			default:
				return nil, errf("unknown key %q (want host, role, dc or weight)", k)
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
			return nil, errf("bad role %q (want validator or rpc; a spare is just a validator at weight 1)", n.Role)
		}
		if weightSet && n.Role != RoleValidator {
			return nil, errf("weight= is only valid on role=validator (rpc nodes are never registered)")
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
