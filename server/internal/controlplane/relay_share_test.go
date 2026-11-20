package controlplane

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/relayshare"
)

// A relay's demand counts for one lease: an unreported or silent relay is no longer a carrier.
func TestRelayDemandBookForgetsSilentRelays(t *testing.T) {
	var book relayDemandBook
	now := time.Now()
	book.record("a", map[string]relayshare.Demand{"acct": {Offered: 100}}, now)
	book.record("b", map[string]relayshare.Demand{"acct": {}}, now.Add(time.Second))
	if got := book.demandFor("acct", now.Add(2*time.Second)); len(got) != 2 {
		t.Fatalf("two relays report the account, got %v", got)
	}
	if got := book.demandFor("other", now.Add(2*time.Second)); len(got) != 0 {
		t.Fatalf("an account no relay reports has carriers: %v", got)
	}
	if got := book.demandFor("acct", now.Add(relayNodeLease+500*time.Millisecond)); len(got) != 1 {
		t.Fatalf("after a lease only b (reported later) is live, got %v", got)
	}
}
