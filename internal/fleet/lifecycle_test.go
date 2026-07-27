package fleet

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/remote/internal/config"
	"github.com/ava-labs/avalanchego/ids"
)

func lifecycleInventoryNodes() []config.Node {
	return []config.Node{
		{Number: 1, Host: "v1", Role: config.RoleValidator, DC: "A"},
		{Number: 2, Host: "v2", Role: config.RoleValidator, DC: "A"},
		{Number: 3, Host: "v3", Role: config.RoleValidator, DC: "B"},
		{Number: 4, Host: "rpc", Role: config.RoleRPC},
	}
}

func TestSelectNodes(t *testing.T) {
	nodes := lifecycleInventoryNodes()
	for _, testCase := range []struct {
		name      string
		selectors []string
		want      []int
		wantError string
	}{
		{name: "no selector is every node", want: []int{1, 2, 3, 4}},
		{name: "node number", selectors: []string{"3"}, want: []int{3}},
		{name: "dc tag", selectors: []string{"dc=A"}, want: []int{1, 2}},
		{name: "union in inventory order", selectors: []string{"4", "dc=B"}, want: []int{3, 4}},
		{name: "overlapping selectors do not duplicate", selectors: []string{"dc=A", "1", "2"}, want: []int{1, 2}},
		{name: "untagged node is only reachable by number", selectors: []string{"4"}, want: []int{4}},
		{name: "unknown node number", selectors: []string{"9"}, wantError: `selector "9" matches no L1 node`},
		{name: "unknown dc tag", selectors: []string{"dc=Z"}, wantError: `selector "dc=Z" matches no L1 node`},
		{name: "empty dc tag", selectors: []string{"dc="}, wantError: "empty dc tag"},
		{name: "malformed selector", selectors: []string{"validator"}, wantError: "must be a node number or dc=<tag>"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := selectNodes(nodes, testCase.selectors)
			if testCase.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
					t.Fatalf("error = %v, want %q", err, testCase.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var numbers []int
			for _, node := range got {
				numbers = append(numbers, node.Number)
			}
			if len(numbers) != len(testCase.want) {
				t.Fatalf("selected %v, want %v", numbers, testCase.want)
			}
			for i := range numbers {
				if numbers[i] != testCase.want[i] {
					t.Fatalf("selected %v, want %v", numbers, testCase.want)
				}
			}
		})
	}
}

func TestSelectNodesRejectsEmptyInventory(t *testing.T) {
	if _, err := selectNodes(nil, nil); err == nil {
		t.Fatal("empty candidate list must be an error")
	}
}

// The P-chain machine is never a candidate, so naming it fails loudly rather
// than silently selecting nothing.
func TestSelectNodesCannotReachPChain(t *testing.T) {
	inv := inventory{nodes: append(lifecycleInventoryNodes(), config.Node{Number: 5, Host: "pchain", Role: config.RolePChain})}
	if _, err := selectNodes(inv.l1Nodes(), []string{"5"}); err == nil || !strings.Contains(err.Error(), "matches no L1 node") {
		t.Fatalf("P-chain selector error = %v", err)
	}
}

func destroyState(chainID ids.ID) deployment {
	return deployment{
		environment: config.FleetEnvironment{SSHUser: "ubuntu", SSHKeyPath: "/key"},
		chainID:     chainID,
		selected: []nodeDeployment{
			{node: config.Node{Number: 1, Host: "v1", Role: config.RoleValidator}},
			{node: config.Node{Number: 2, Host: "v2", Role: config.RoleValidator}},
		},
	}
}

func TestDestroyKillsEveryNodeBeforeDeletingAnyData(t *testing.T) {
	chainID := ids.GenerateTestID()
	runner := &recordingRunner{}
	deployer := &Deployer{root: t.TempDir(), out: io.Discard, runner: runner}
	if err := deployer.destroyPhases(context.Background(), destroyState(chainID)); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 4 {
		t.Fatalf("commands = %d, want 4: %v", len(runner.runs), runner.runs)
	}
	var kills, deletes []int
	for index, command := range runner.runs {
		joined := strings.Join(command, " ")
		if strings.Contains(joined, "SIGKILL") {
			kills = append(kills, index)
			if !strings.Contains(joined, "is-active") {
				t.Fatalf("kill does not verify inactivity: %q", joined)
			}
		}
		if strings.Contains(joined, "rm -rf") {
			deletes = append(deletes, index)
			if !strings.Contains(joined, "/chainData/"+chainID.String()) {
				t.Fatalf("delete is not scoped to this L1 chain data: %q", joined)
			}
			if strings.Contains(joined, "/db") || strings.Contains(joined, "/logs") ||
				strings.Contains(joined, remoteConfigDir) || strings.Contains(joined, remotePackageDir) {
				t.Fatalf("delete removed preserved state: %q", joined)
			}
		}
	}
	if len(kills) != 2 || len(deletes) != 2 {
		t.Fatalf("kills = %v, deletes = %v", kills, deletes)
	}
	if kills[len(kills)-1] > deletes[0] {
		t.Fatalf("a delete ran before every kill finished: %v", runner.runs)
	}
}

func TestDestroyDeletesNothingWhenAnyUnitIsNotInactive(t *testing.T) {
	for name, failing := range map[string]int{
		"first node fails": 0,
		"last node fails":  1,
	} {
		t.Run(name, func(t *testing.T) {
			runner := &recordingRunner{runErrors: map[int]error{failing: errors.New("unit still active")}}
			deployer := &Deployer{root: t.TempDir(), out: io.Discard, runner: runner}
			err := deployer.destroyPhases(context.Background(), destroyState(ids.GenerateTestID()))
			if err == nil || !strings.Contains(err.Error(), "destroy kill phase") {
				t.Fatalf("destroy error = %v", err)
			}
			for _, command := range runner.runs {
				if strings.Contains(strings.Join(command, " "), "rm -rf") {
					t.Fatalf("destroy deleted data despite a failed kill: %v", runner.runs)
				}
			}
		})
	}
}

// Re-pushing the identity on every start is deliberate: control-side placement
// must always win over whatever key the machine currently holds.
func TestStartRepushesIdentityBetweenStopAndStart(t *testing.T) {
	runner := &recordingRunner{}
	deployer := &Deployer{root: t.TempDir(), out: io.Discard, runner: runner}
	state := deployment{
		environment: config.FleetEnvironment{SSHUser: "ubuntu", SSHKeyPath: "/key"},
		selected: []nodeDeployment{
			{node: config.Node{Number: 1, Host: "v1", Role: config.RoleValidator}},
		},
	}
	if err := deployer.runPhases(context.Background(), state, []lifecyclePhase{
		{"stop", deployer.stop},
		{"identity", deployer.installIdentity},
		{"start", deployer.enableAndStart},
	}); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(runner.runs[0], " ")
	if !strings.Contains(joined, "systemctl stop") || !strings.Contains(joined, "is-active") {
		t.Fatalf("start did not stop and wait for inactivity first: %q", joined)
	}
	last := strings.Join(runner.runs[len(runner.runs)-1], " ")
	if !strings.Contains(last, "systemctl enable") || !strings.Contains(last, "systemctl start") {
		t.Fatalf("start phase did not enable and start last: %q", last)
	}
	all := ""
	for _, command := range runner.runs {
		all += strings.Join(command, " ") + "\n"
	}
	if !strings.Contains(all, "staking") || !strings.Contains(all, "staker.key") {
		t.Fatalf("start did not re-push the assigned identity: %s", all)
	}
}

// Stop is graceful and turns off boot intent, and never touches data.
func TestStopDisablesGracefullyAndPreservesData(t *testing.T) {
	runner := &recordingRunner{}
	deployer := &Deployer{root: t.TempDir(), out: io.Discard, runner: runner}
	state := destroyState(ids.GenerateTestID())
	if err := deployer.runPhases(context.Background(), state, []lifecyclePhase{{"stop", deployer.disableAndStop}}); err != nil {
		t.Fatal(err)
	}
	if len(runner.runs) != 2 {
		t.Fatalf("commands = %d, want 2: %v", len(runner.runs), runner.runs)
	}
	for _, command := range runner.runs {
		joined := strings.Join(command, " ")
		if !strings.Contains(joined, "systemctl disable") ||
			!strings.Contains(joined, "systemctl stop") ||
			!strings.Contains(joined, "is-active") {
			t.Fatalf("stop is not disable plus graceful stop plus inactivity wait: %q", joined)
		}
		if strings.Contains(joined, "SIGKILL") || strings.Contains(joined, "rm -rf") {
			t.Fatalf("stop killed or deleted something: %q", joined)
		}
	}
}
