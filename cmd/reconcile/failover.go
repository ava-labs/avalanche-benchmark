package main

func siteName(site int) string {
	if site == siteB {
		return "b"
	}
	return "a"
}

// rpcMachineIdxs lists the pool indexes of a site's pinned RPC slots.
func rpcMachineIdxs(t Topology, site int) []int {
	var idxs []int
	for i := 0; i < t.Size(); i++ {
		if t.Site(i) == site && t.IsRPCSlot(i) {
			idxs = append(idxs, i)
		}
	}
	return idxs
}
