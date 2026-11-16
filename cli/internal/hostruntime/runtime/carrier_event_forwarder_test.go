//go:build darwin || linux || windows

package runtime

import (
	"context"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
)

func TestWorkerForwardsCarrierRefreshAndRecovery(t *testing.T) {
	hub, _, plane := eventstest.Start(t, time.Hour, carrierRenewalTopics...)
	carrier := &ProductionCarrier{events: machineevents.NewPushed()}
	network := carrier.events.Subscribe(carrierRenewalTopics...)
	forwarder := newCarrierEventForwarder(hub, func(_ context.Context, refresh, healthy bool) error {
		carrier.EventsHealth(healthy)
		if refresh {
			carrier.RefreshNetwork()
		}
		return nil
	})
	if err := forwarder.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer forwarder.Shutdown(context.Background())
	await := func(ch <-chan struct{}) {
		t.Helper()
		select {
		case <-ch:
		case <-time.After(3 * time.Second):
			t.Fatal("forwarded event not delivered")
		}
	}
	await(network.Changed()) // initial refresh
	for _, topic := range carrierRenewalTopics {
		plane.Publish(topic)
		await(network.Changed())
	}
	select {
	case <-carrier.events.Recovered():
	default:
	}
	plane.FailNext(1)
	plane.Publish(machineevents.TopicPeerNetwork)
	await(forwarder.recovered)
	await(carrier.events.Recovered())
}
