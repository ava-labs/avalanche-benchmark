package fleet

import (
	"context"
	"io"
	"strings"
	"testing"
)

func TestFreezeGateRefusesWithExactReason(t *testing.T) {
	for _, testCase := range []struct {
		name string
		gate freezeGate
		want []string
	}{
		{
			name: "ready",
			gate: freezeGate{upstreamHeight: 100, localHeight: 100, managerVisible: true, mainVisible: true},
		},
		{
			name: "behind upstream",
			gate: freezeGate{upstreamHeight: 289700, localHeight: 289600, managerVisible: true, mainVisible: true},
			want: []string{"not synchronized", "289600", "289700", "lag 100"},
		},
		{
			name: "management set missing",
			gate: freezeGate{upstreamHeight: 100, localHeight: 101, managerVisible: false, mainVisible: true},
			want: []string{"missing the management validator set", "101", "100"},
		},
		{
			name: "main set missing",
			gate: freezeGate{upstreamHeight: 100, localHeight: 100, managerVisible: true, mainVisible: false},
			want: []string{"missing the main validator set"},
		},
		{
			name: "both sets missing",
			gate: freezeGate{upstreamHeight: 100, localHeight: 100},
			want: []string{"missing the management and main validator set"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := testCase.gate.check()
			if len(testCase.want) == 0 {
				if err != nil {
					t.Fatalf("gate refused a ready node: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("gate accepted an unready node")
			}
			for _, fragment := range testCase.want {
				if !strings.Contains(err.Error(), fragment) {
					t.Fatalf("refusal %q does not report %q", err, fragment)
				}
			}
		})
	}
}

func TestPChainModeFromRenderedConfig(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		config string
		want   string
	}{
		{"frozen", `{"p-chain-follow-only":true,"bootstrap-ips":"","bootstrap-ids":""}`, frozenMode},
		{"following", `{"p-chain-follow-only":true}`, followMode},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			deployer := &Deployer{out: io.Discard, runner: &recordingRunner{output: []byte(testCase.config)}}
			got, err := deployer.probePChainMode(context.Background(), deployment{}, nodeDeployment{})
			if err != nil {
				t.Fatal(err)
			}
			if got != testCase.want {
				t.Fatalf("mode = %q, want %q", got, testCase.want)
			}
		})
	}

	deployer := &Deployer{out: io.Discard, runner: &recordingRunner{output: []byte("no such file")}}
	if _, err := deployer.probePChainMode(context.Background(), deployment{}, nodeDeployment{}); err == nil {
		t.Fatal("undecodable remote configuration must fail loudly")
	}
}
