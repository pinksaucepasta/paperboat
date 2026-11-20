package derpquic

import "testing"

// The account budget charges bytes (at least accountMinimumCost); the carrier budget keeps charging a full fragment for any datagram, so
// a flood of tiny frames from one peer is still bounded.
func TestAccountBudgetChargesBytesWhereTheCarrierBudgetChargesAFragment(t *testing.T) {
	account, carrier := newAccountBudget(1<<20), newBudget(carrierBytesPerSecond)
	if got := account.charge(60); got != accountMinimumCost {
		t.Fatalf("account charge for a 60-byte ACK = %v, want %d", got, accountMinimumCost)
	}
	if got := account.charge(1067); got != 1067 {
		t.Fatalf("account charge for a full datagram = %v, want 1067", got)
	}
	if got := carrier.charge(60); got != budgetMinimumCost {
		t.Fatalf("carrier charge for a 60-byte datagram = %v, want %d", got, budgetMinimumCost)
	}
}
