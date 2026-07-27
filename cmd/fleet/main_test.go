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
		"benchmark-fleet deploy <frozen|follow>",
		"benchmark-fleet pchain <archive|follow|freeze>",
		"benchmark-fleet status",
		"benchmark-fleet place <identity-letter> <node>",
		"benchmark-fleet apply-placement",
	} {
		if !strings.Contains(message, expected) {
			t.Fatalf("usage %q does not contain %q", message, expected)
		}
	}
}
