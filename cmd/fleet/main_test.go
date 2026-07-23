package main

import (
	"os"
	"strings"
	"testing"
)

func TestUsageUsesExecutableName(t *testing.T) {
	oldArgs := os.Args
	t.Cleanup(func() { os.Args = oldArgs })
	os.Args = []string{"/tmp/benchmark-fleet"}

	err := run()
	if err == nil {
		t.Fatal("expected usage error")
	}
	message := err.Error()
	for _, expected := range []string{
		"benchmark-fleet pchain follow",
		"benchmark-fleet pchain freeze",
		"benchmark-fleet pchain status",
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("usage %q does not contain %q", message, expected)
		}
	}
}
