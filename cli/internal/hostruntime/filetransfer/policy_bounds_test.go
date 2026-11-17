package filetransfer

import "testing"

// With no server policy yet the host is bounded by the Automatic bound (2,000,000,000), never unbounded; a policy
// without automatic_max_batch_bytes is invalid and falls back to it.
func TestPolicyIsBoundedByTheAutomaticBatchBound(t *testing.T) {
	if !DefaultPolicy.Valid() {
		t.Fatal("DefaultPolicy is not valid")
	}
	if file, batch := DefaultPolicy.Bounds(); file != AutomaticTransferLimit || batch != AutomaticTransferLimit {
		t.Fatalf("bounds=%d/%d", file, batch)
	}
	if got := NewPolicyStore(Policy{}).Current(); got.AutomaticMaxBatchBytes != AutomaticTransferLimit {
		t.Fatalf("a store without a valid policy bounds at %d, want the Automatic bound", got.AutomaticMaxBatchBytes)
	}
	over := DefaultPolicy
	over.AutomaticMaxBatchBytes = AutomaticTransferLimit + 1
	smallSpool := DefaultPolicy
	smallSpool.MaxPendingSpoolBytes = 1 << 30
	none := DefaultPolicy
	none.AutomaticMaxBatchBytes = 0
	if over.Valid() || smallSpool.Valid() || none.Valid() {
		t.Fatal("accepted a bound over 2,000,000,000, a spool smaller than the bound, or no bound")
	}
}
