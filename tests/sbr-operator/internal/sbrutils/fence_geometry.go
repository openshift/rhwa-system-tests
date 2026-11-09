package sbrutils

// FenceGeometryVictim picks the highest eligible node ID whose following slot is
// unassigned. A lower ID with a gap preserves the off-by-one regression check.
// eligible contains only nodes with an agent that can safely be rebooted.
func FenceGeometryVictim(nodeIDs map[string]uint16, eligible map[string]bool) (string, uint16) {
	assigned := make(map[uint32]bool, len(nodeIDs))
	for _, nodeID := range nodeIDs {
		assigned[uint32(nodeID)] = true
	}
	var victim string
	var victimID uint16
	for nodeName, nodeID := range nodeIDs {
		if !eligible[nodeName] || nodeID == 0 || assigned[uint32(nodeID)+1] {
			continue
		}
		if nodeID > victimID {
			victim, victimID = nodeName, nodeID
		}
	}

	return victim, victimID
}
