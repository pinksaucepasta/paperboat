package filetransfer

import "testing"

// PLAN 3b File transfer modes: the server replaces max_file_bytes/max_batch_bytes with one
// automatic_max_batch_bytes (2,000,000,000) and raises the spool to it; the old form stays valid until it stops.
func TestPolicyAcceptsTheAutomaticBatchBound(t *testing.T) {
	automatic := DefaultPolicy
	automatic.MaxFileBytes, automatic.MaxBatchBytes = 0, 0
	automatic.AutomaticMaxBatchBytes, automatic.MaxPendingSpoolBytes = AutomaticTransferLimit, AutomaticTransferLimit
	if !automatic.Valid() || !DefaultPolicy.Valid() {
		t.Fatalf("automatic=%v legacy=%v, want both valid", automatic.Valid(), DefaultPolicy.Valid())
	}
	if file, batch := automatic.Bounds(); file != AutomaticTransferLimit || batch != AutomaticTransferLimit {
		t.Fatalf("bounds=%d/%d", file, batch)
	}
	over := automatic
	over.AutomaticMaxBatchBytes = AutomaticTransferLimit + 1
	smallSpool := automatic
	smallSpool.MaxPendingSpoolBytes = 1 << 30
	if over.Valid() || smallSpool.Valid() {
		t.Fatal("accepted a bound over 2,000,000,000 or a spool smaller than the bound")
	}
}
