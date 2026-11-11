package machineevents

import "testing"

// Waiting hosts are answered by notifications; the re-read backstop must stay a single query per held wait, or N idle
// daemons cost N/5 queries per second again.
func TestRecheckBackstopFiresAtMostOncePerWait(t *testing.T) {
	if MaxWait >= 2*recheckInterval {
		t.Fatalf("recheckInterval %v fires %d times in a MaxWait %v wait", recheckInterval, int(MaxWait/recheckInterval), MaxWait)
	}
	if MaxWait >= ClientHeaderTimeout {
		t.Fatalf("MaxWait %v must stay under the client header timeout %v", MaxWait, ClientHeaderTimeout)
	}
}
