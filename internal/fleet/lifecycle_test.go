package fleet

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/ava-labs/avalanche-benchmark/internal/config"
	"github.com/ava-labs/avalanche-benchmark/internal/creation"
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
		{name: "union in inventory order", selectors: []string{"4", "3"}, want: []int{3, 4}},
		{name: "overlapping selectors do not duplicate", selectors: []string{"1", "1", "2"}, want: []int{1, 2}},
		{name: "untagged node is only reachable by number", selectors: []string{"4"}, want: []int{4}},
		{name: "unknown node number", selectors: []string{"9"}, wantError: `selector "9" matches no L1 node`},
		// dc= is gone on purpose: one command must not be able to take a whole
		// site down. The rejection is explicit rather than "not a number", so an
		// operator reaching for the old syntax learns why.
		{name: "dc tag is rejected", selectors: []string{"dc=A"}, wantError: "dc= selectors were removed"},
		{name: "empty dc tag is rejected", selectors: []string{"dc="}, wantError: "dc= selectors were removed"},
		{name: "dc tag inside a comma list is rejected", selectors: []string{"1,dc=B"}, wantError: "dc= selectors were removed"},
		{name: "malformed selector", selectors: []string{"validator"}, wantError: "must be a node number"},
		{name: "comma separated", selectors: []string{"1,3,4"}, want: []int{1, 3, 4}},
		{name: "comma mixed with separate args", selectors: []string{"1,2", "3"}, want: []int{1, 2, 3}},
		{name: "comma with spaces and trailing comma", selectors: []string{" 1 , 3 ,"}, want: []int{1, 3}},
		{name: "only commas", selectors: []string{",,"}, wantError: "contain no node number"},
		{name: "comma still rejects garbage", selectors: []string{"1,validator"}, wantError: "must be a node number"},
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
		environment: config.FleetEnvironment{SSHUser: "ubuntu", SSHKeyPath: "/key", SystemInstall: true},
		chainIDs:    map[string]ids.ID{"main": chainID},
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
		environment: config.FleetEnvironment{SSHUser: "ubuntu", SSHKeyPath: "/key", SystemInstall: true},
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

// start must be idempotent: a node already serving its assigned identity is
// left strictly alone. Restarting a healthy node drops its peers and sends it
// back behind the 75% connected-stake gate for nothing.
func TestPlanStartOnlyRestartsWhatIsBroken(t *testing.T) {
	target := nodeDeployment{
		node:     config.Node{Number: 5},
		identity: creation.PublicNode{Identity: "a", NodeID: "NodeID-A"},
	}
	for _, testCase := range []struct {
		name        string
		active      bool
		runtimeID   string
		wantRestart bool
		wantNoteHas string
	}{
		{name: "running the right identity is untouched", active: true, runtimeID: "NodeID-A",
			wantRestart: false, wantNoteHas: "leaving it alone"},
		{name: "running the wrong identity restarts", active: true, runtimeID: "NodeID-B",
			wantRestart: true, wantNoteHas: "placement assigns identity a"},
		{name: "running but API silent restarts", active: true, runtimeID: "",
			wantRestart: true, wantNoteHas: "does not answer"},
		{name: "down starts without a note", active: false, runtimeID: "",
			wantRestart: true, wantNoteHas: ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			restart, note := planStart(target, testCase.active, testCase.runtimeID)
			if restart != testCase.wantRestart {
				t.Fatalf("restart = %v, want %v", restart, testCase.wantRestart)
			}
			if testCase.wantNoteHas == "" {
				if note != "" {
					t.Fatalf("note = %q, want empty", note)
				}
				return
			}
			if !strings.Contains(note, testCase.wantNoteHas) {
				t.Fatalf("note = %q, want it to contain %q", note, testCase.wantNoteHas)
			}
		})
	}
}

// pchainLifecycleInventory extends the placement fixture with the addressing
// fields the P-chain lifecycle verbs resolve through.
func pchainLifecycleInventory(t *testing.T, environment config.FleetEnvironment) inventory {
	t.Helper()
	inv := placementTestInventory(t)
	inv.environment = environment
	inv.ports = portsByNode(inv.nodes)
	for _, node := range inv.nodes {
		if node.Role == config.RolePChain {
			inv.pchain = node
		}
	}
	return inv
}

// fleet pchain start|stop exist for the airgapped frozen fleet after a host
// reboot, so they must talk ONLY to the P-chain machine itself: no upstream
// API, no reinstall, no other host.
func TestPChainStartAndStopTouchOnlyThePChainMachine(t *testing.T) {
	for name, environment := range map[string]config.FleetEnvironment{
		"system": {SSHUser: "ubuntu", SSHKeyPath: "/key", SystemInstall: true},
		"user":   {SSHUser: "op", SSHKeyPath: "/key", RemoteDir: "/home/op/kit"},
	} {
		t.Run(name, func(t *testing.T) {
			inv := pchainLifecycleInventory(t, environment)

			runner := &recordingRunner{}
			deployer := &Deployer{root: t.TempDir(), out: io.Discard, runner: runner}
			if err := deployer.startPChain(context.Background(), inv); err != nil {
				t.Fatal(err)
			}
			if len(runner.runs) != 2 {
				t.Fatalf("start commands = %d, want start plus liveness check: %v", len(runner.runs), runner.runs)
			}
			for _, command := range runner.runs {
				joined := strings.Join(command, " ")
				if !strings.Contains(joined, "@pchain") {
					t.Fatalf("start reached a machine other than the P-chain node: %q", joined)
				}
				if environment.RemoteDir != "" && (strings.Contains(joined, "sudo") || strings.Contains(joined, "systemctl")) {
					t.Fatalf("user-level pchain start reaches for root: %q", joined)
				}
			}

			runner = &recordingRunner{}
			deployer = &Deployer{root: t.TempDir(), out: io.Discard, runner: runner}
			if err := deployer.stopPChain(context.Background(), inv); err != nil {
				t.Fatal(err)
			}
			if len(runner.runs) != 1 {
				t.Fatalf("stop commands = %d, want 1: %v", len(runner.runs), runner.runs)
			}
			joined := strings.Join(runner.runs[0], " ")
			if !strings.Contains(joined, "@pchain") {
				t.Fatalf("stop reached a machine other than the P-chain node: %q", joined)
			}
			if strings.Contains(joined, "SIGKILL") && environment.RemoteDir == "" {
				t.Fatalf("pchain stop is not graceful: %q", joined)
			}
			if strings.Contains(joined, "rm -rf") {
				t.Fatalf("pchain stop deleted something: %q", joined)
			}
		})
	}
}

// A failed liveness check must be an error: "started" with a dead process is
// how a reboot recovery silently fails.
func TestPChainStartFailsWhenTheProcessDoesNotStayUp(t *testing.T) {
	inv := pchainLifecycleInventory(t, config.FleetEnvironment{SSHUser: "ubuntu", SSHKeyPath: "/key", SystemInstall: true})
	runner := &recordingRunner{runErrors: map[int]error{1: errors.New("inactive")}}
	deployer := &Deployer{root: t.TempDir(), out: io.Discard, runner: runner}
	err := deployer.startPChain(context.Background(), inv)
	if err == nil || !strings.Contains(err.Error(), "did not stay up") {
		t.Fatalf("start error = %v, want a liveness failure", err)
	}
}

func TestSelectorsNameHonoursCommaSplitting(t *testing.T) {
	if !selectorsName([]string{"1,13"}, 13) || !selectorsName([]string{" 13 "}, 13) {
		t.Fatal("selectorsName missed the P-chain number")
	}
	if selectorsName([]string{"1,12"}, 13) || selectorsName([]string{"dc=A"}, 13) {
		t.Fatal("selectorsName matched selectors that do not name the node")
	}
}

// destroy deletes chain data, so it must never have a bare form meaning
// "every node". The error must arrive even when the inventory cannot be read,
// because the whole-fleet hint is a convenience, not a precondition.
func TestDestroyRequiresExplicitNodes(t *testing.T) {
	deployer := NewDeployer(t.TempDir(), io.Discard)
	err := deployer.Destroy(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "requires explicit node numbers") {
		t.Fatalf("bare destroy error = %v, want a demand for explicit node numbers", err)
	}
}
