package relaylimits

import "testing"

// The ceiling must stay above what one account-heavy node needs (thousands of machines) and
// within the worst-case memory budget of one relay box.
func TestMaxConnectionsPerNodeFitsAWorstCaseNode(t *testing.T) {
	const worstCaseBytesPerConnection = 256 << 10
	if MaxConnectionsPerNode < 4096 || MaxConnectionsPerNode*worstCaseBytesPerConnection > 4<<30 {
		t.Fatalf("MaxConnectionsPerNode = %d", MaxConnectionsPerNode)
	}
}
