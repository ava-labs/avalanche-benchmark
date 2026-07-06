package valmgr

import (
	"testing"

	"github.com/ava-labs/avalanchego/ids"
)

func TestConversionIndices(t *testing.T) {
	// Slot order deliberately NOT in NodeID order.
	nodeIDs := []ids.NodeID{
		{3, 0, 0}, // slot 0 -> sorts 3rd
		{1, 0, 0}, // slot 1 -> sorts 1st
		{2, 0, 0}, // slot 2 -> sorts 2nd
		{9, 0, 0}, // slot 3 -> sorts 4th
	}
	got := ConversionIndices(nodeIDs)
	want := []int{2, 0, 1, 3}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ConversionIndices = %v, want %v", got, want)
		}
	}
}

func TestValidationIDMatchesAppend(t *testing.T) {
	subnetID := ids.GenerateTestID()
	if ValidationID(subnetID, 3) != subnetID.Append(3) {
		t.Error("ValidationID must be subnetID.Append(index)")
	}
}
