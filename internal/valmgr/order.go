package valmgr

import (
	"bytes"
	"sort"

	"github.com/ava-labs/avalanchego/ids"
)

// ConversionIndices maps staking slots to conversion-tx validator indices.
//
// ConvertSubnetToL1Tx requires its validators sorted by NodeID bytes (the
// wallet builder sorts them regardless of input order), and a conversion-time
// validator's validationID is sha256(subnetID || uint32BE(position in the
// sorted tx list)). Given the staking slots' NodeIDs in slot order, this
// returns conv where conv[k] is the conversion index of the k-th staking
// slot. Deterministic from node-ids.env, so create-l1 and reconcile always
// agree on every validationID.
func ConversionIndices(nodeIDs []ids.NodeID) []int {
	order := make([]int, len(nodeIDs))
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(a, b int) bool {
		return bytes.Compare(nodeIDs[order[a]].Bytes(), nodeIDs[order[b]].Bytes()) < 0
	})
	conv := make([]int, len(nodeIDs))
	for pos, k := range order {
		conv[k] = pos
	}
	return conv
}
