package topo

import (
	"strings"
	"testing"
)

func TestParseBasics(t *testing.T) {
	nodes, err := Parse(`
# the fleet
a1     host=198.51.100.11   role=validator  dc=A
rpc_a1 host=198.51.100.12  role=rpc        dc=A  # trailing comment
b1     host=198.51.100.13  role=validator
x9     host=198.51.100.13  role=validator
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 4 {
		t.Fatalf("got %d nodes, want 4", len(nodes))
	}
	a1 := nodes[0]
	if a1.Name != "a1" || a1.Host != "198.51.100.11" || a1.Role != RoleValidator ||
		a1.DC != "A" || !a1.IsValidator() {
		t.Errorf("a1 = %+v", a1)
	}
	if rpc := nodes[1]; rpc.Role != RoleRPC || rpc.IsValidator() || rpc.DC != "A" {
		t.Errorf("rpc_a1 = %+v", rpc)
	}
	if b1 := nodes[2]; b1.DC != "" {
		t.Errorf("b1 = %+v (dc must default empty)", b1)
	}
	if got := Validators(nodes); len(got) != 3 || got[2].Name != "x9" {
		t.Errorf("Validators = %v", got)
	}
}

func TestParsePositionalPorts(t *testing.T) {
	nodes, err := Parse(`
a1 host=10.0.0.1 role=validator
b1 host=10.0.0.2 role=validator
x9 host=10.0.0.1 role=validator
x10 host=10.0.0.1 role=rpc
`)
	if err != nil {
		t.Fatal(err)
	}
	// First node on a host: 9650/9651; later nodes on the SAME host step +2
	// in file order. Distinct hosts each start at the base.
	want := []struct{ port, p2p int }{{9650, 9651}, {9650, 9651}, {9652, 9653}, {9654, 9655}}
	for i, w := range want {
		if nodes[i].Port != w.port || nodes[i].StakingPort() != w.p2p {
			t.Errorf("%s ports = %d/%d, want %d/%d", nodes[i].Name, nodes[i].Port, nodes[i].StakingPort(), w.port, w.p2p)
		}
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]string{
		"a1 host=1.2.3.4 role=validator\na1 host=1.2.3.5 role=rpc": "duplicate node name",
		"a1 role=validator":          "host= is required",
		"a1 host=1.2.3.4":            "role= is required",
		"a1 host=1.2.3.4 role=spare": "bad role",
		// weight= was an inventory key once; it is gone, weights live on-chain.
		"a1 host=1.2.3.4 role=validator weight=1000": "unknown key",
		"a1 host=1.2.3.4 role=validator color=red":   "unknown key",
		"a1 host=1.2.3.4 role=validator port=9660":   "unknown key",
		"a1 host":                   "bad field",
		"a=1 host=1.2.3.4 role=rpc": "bad node name",
		"":                          "no nodes defined",
		"# only comments\n\n   \n":  "no nodes defined",
	}
	for src, wantErr := range cases {
		_, err := Parse(src)
		if err == nil || !strings.Contains(err.Error(), wantErr) {
			t.Errorf("Parse(%q) err = %v, want %q", src, err, wantErr)
		}
	}
}
