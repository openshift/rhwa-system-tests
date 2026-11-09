package sbrutils

import "testing"

func TestFenceGeometryVictim(t *testing.T) {
	testCases := []struct {
		name     string
		nodeIDs  map[string]uint16
		eligible map[string]bool
		wantNode string
		wantID   uint16
	}{
		{
			name:     "highest ID is a controller, lower worker has an unused successor",
			nodeIDs:  map[string]uint16{"controller": 220, "worker": 79, "peer": 40},
			eligible: map[string]bool{"worker": true}, wantNode: "worker", wantID: 79,
		},
		{
			name:     "reject a worker whose next slot belongs to a controller",
			nodeIDs:  map[string]uint16{"controller": 80, "worker": 79, "other": 40},
			eligible: map[string]bool{"worker": true, "other": true}, wantNode: "other", wantID: 40,
		},
		{
			name:     "prefer the highest eligible ID with an unused successor",
			nodeIDs:  map[string]uint16{"controller": 220, "worker": 79, "other": 40},
			eligible: map[string]bool{"worker": true, "other": true}, wantNode: "worker", wantID: 79,
		},
		{
			name:     "last slot in the region remains eligible",
			nodeIDs:  map[string]uint16{"worker": 255, "peer": 40},
			eligible: map[string]bool{"worker": true}, wantNode: "worker", wantID: 255,
		},
		{
			name:    "no eligible nodes",
			nodeIDs: map[string]uint16{"controller": 220}, eligible: map[string]bool{},
		},
		{
			name:     "all eligible nodes have occupied successors",
			nodeIDs:  map[string]uint16{"controller": 81, "worker": 79, "other": 80},
			eligible: map[string]bool{"worker": true, "other": true},
		},
		{
			name:    "unassigned node ID is never a victim",
			nodeIDs: map[string]uint16{"worker": 0}, eligible: map[string]bool{"worker": true},
		},
		{name: "empty node map"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			node, nodeID := FenceGeometryVictim(testCase.nodeIDs, testCase.eligible)
			if node != testCase.wantNode || nodeID != testCase.wantID {
				t.Fatalf("victim = (%q, %d), want (%q, %d)", node, nodeID, testCase.wantNode, testCase.wantID)
			}
		})
	}
}
