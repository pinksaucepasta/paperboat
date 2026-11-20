package db

import "testing"

func TestRolloutInstancesAlwaysFitMaxConnections(t *testing.T) {
	// A rollout plus a rollback can run RolloutInstances servers at their peak at once; none may exceed the server's limit.
	for usable := 16; usable <= 500; usable++ {
		request, direct := fitPoolSizes(20, 6, usable)
		if peak := RolloutInstances * int(request+direct+migrationConns); peak > usable {
			t.Fatalf("usable %d: pools %d+%d+%d on %d servers need %d", usable, request, direct, migrationConns, RolloutInstances, peak)
		}
		if request < 1 || direct < 1 {
			t.Fatalf("usable %d: pools %d+%d leave a pool empty", usable, request, direct)
		}
	}
	// prod: max_connections 100, 3 reserved. It used to need 4 x 26 = 104.
	if request, direct := fitPoolSizes(20, 6, 97); request != 16 || direct != 6 {
		t.Fatalf("prod sizes = %d+%d, want 16+6", request, direct)
	}
	// A roomy server keeps the configured sizes.
	if request, direct := fitPoolSizes(20, 6, 400); request != 20 || direct != 6 {
		t.Fatalf("roomy sizes = %d+%d, want 20+6", request, direct)
	}
}
