package main

import (
	"os"
	"syscall"
	"testing"
)

type fakeSignal string

func (fakeSignal) Signal() {}

func (s fakeSignal) String() string {
	return string(s)
}

func TestSignalExitCode(t *testing.T) {
	tests := []struct {
		name string
		sig  os.Signal
		want int
	}{
		{name: "sigint", sig: syscall.SIGINT, want: 130},
		{name: "sigterm", sig: syscall.SIGTERM, want: 143},
		{name: "unknown", sig: fakeSignal("custom"), want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := signalExitCode(tt.sig)
			if got != tt.want {
				t.Fatalf("signalExitCode(%v) = %d, want %d", tt.sig, got, tt.want)
			}
		})
	}
}
