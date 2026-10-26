package runtime

import (
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
)

// F5-10: the carrier's renewal wakes on an access change but never on what a renewal itself moves (it reports
// endpoints, which publishes peer_network and inventory): 46aa1e11 listened to those and renewals fed each other.
func TestCarrierRenewalWakesOnAccessChangesNotOnItsOwnEffects(t *testing.T) {
	hub, _, plane := eventstest.Start(t, time.Hour, machineevents.TopicMachineAccess)
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
	plane.Publish(machineevents.TopicPeerNetwork)
	plane.Publish(machineevents.TopicInventory)
	if woke() {
		t.Fatal("a renewal's own peer_network/inventory publish woke the carrier's renewal")
	}
	plane.Publish(machineevents.TopicMachineAccess)
	if !woke() {
		t.Fatal("an access change did not wake the carrier's renewal")
	}
}
