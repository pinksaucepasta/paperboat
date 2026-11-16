package runtime

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
)

// F21-18: the carrier renews on an access or peer network change, so it needs no per-minute fallback, but not on
// inventory, which a renewal's own effects could move (46aa1e11 listened to peer_network and inventory when every
// unchanged endpoint report still published them; the server publishes only a changed set now).
func TestCarrierRenewalWakesOnAccessAndPeerNetworkChangesNotInventory(t *testing.T) {
	hub, _, plane := eventstest.Start(t, time.Hour, machineevents.TopicMachineAccess, machineevents.TopicPeerNetwork)
	sub := hub.Subscribe(carrierRenewalTopics...)
	woke := func() bool {
		select {
		case <-sub.Changed():
			return true
		case <-time.After(300 * time.Millisecond):
			return false
		}
	}
	woke() // a subscription to a newly known topic may wake once
	plane.Publish(machineevents.TopicInventory)
	if woke() {
		t.Fatal("an inventory change woke the carrier's renewal")
	}
	for _, topic := range []machineevents.Topic{machineevents.TopicMachineAccess, machineevents.TopicPeerNetwork} {
		plane.Publish(topic)
		if !woke() {
			t.Fatalf("a %s change did not wake the carrier's renewal", topic)
		}
	}
}

// Inventory churn anywhere in an account (renames, capabilities, leases) wakes no host renewal however much of it
// there is; a peer_network change (keys, revocation, a machine entering or leaving the mesh) still does at once.
func TestHostRenewalIgnoresInventoryChurnButNotPeerNetwork(t *testing.T) {
	hub, _, plane := eventstest.Start(t, time.Hour, machineevents.TopicPeerNetwork, machineevents.TopicInventory)
	sub := hub.Subscribe(hostRenewalTopics...)
	woke := func() bool {
		select {
		case <-sub.Changed():
			return true
		case <-time.After(300 * time.Millisecond):
			return false
		}
	}
	woke() // a subscription to a newly known topic may wake once
	for range 50 {
		plane.Publish(machineevents.TopicInventory)
	}
	if woke() {
		t.Fatal("inventory churn woke the host's renewal")
	}
	plane.Publish(machineevents.TopicPeerNetwork)
	if !woke() {
		t.Fatal("a peer_network change did not wake the host's renewal")
	}
}
