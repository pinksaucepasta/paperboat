//go:build darwin || linux || windows

package runtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents/eventstest"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
)

func TestWorkerForwardsDaemonTopicsFromSingleWait(t *testing.T) {
	hub, _, plane := eventstest.Start(t, time.Hour, machineevents.DaemonTopics...)
	var refuse atomic.Bool
	pushed := make(chan localapi.MachineEventPush, 16)
	forwarder := newDaemonEventForwarder(hub, "machine_test", func(_ context.Context, push localapi.MachineEventPush) error {
		if refuse.Load() {
			return errors.New("daemon restarting")
		}
		pushed <- push
		return nil
	})
	if err := forwarder.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer forwarder.Shutdown(context.Background())
	next := func() localapi.MachineEventPush {
		t.Helper()
		select {
		case push := <-pushed:
			return push
		case <-time.After(3 * time.Second):
			t.Fatal("daemon missed worker push")
			return localapi.MachineEventPush{}
		}
	}
	next() // initial heartbeat
	for _, topic := range machineevents.DaemonTopics {
		plane.Publish(topic)
		push := next()
		if push.MachineID != "machine_test" || len(push.Topics) != 1 || push.Topics[0] != topic {
			t.Fatalf("push = %+v; want %s only", push, topic)
		}
	}
	// A failed local delivery must remain pending when another topic wakes the
	// forwarder; otherwise a daemon restart loses invalidations until safety polls.
	refuse.Store(true)
	plane.Publish(machineevents.TopicRecipe)
	deadline := time.Now().Add(3 * time.Second)
	for hub.Generations()[machineevents.TopicRecipe] < 2 {
		if time.Now().After(deadline) {
			t.Fatal("worker did not see recipe change")
		}
		time.Sleep(time.Millisecond)
	}
	refuse.Store(false)
	plane.Publish(machineevents.TopicInventory)
	found := map[machineevents.Topic]bool{}
	for !found[machineevents.TopicRecipe] || !found[machineevents.TopicInventory] {
		for _, topic := range next().Topics {
			found[topic] = true
		}
	}
	// An unauthorized wait wakes caches even when no generation moved.
	plane.FailNextWith(1, 401)
	gotAll := false
	for !gotAll {
		push := next()
		gotAll = len(push.Topics) == len(machineevents.DaemonTopics)
	}
	// Every server request belongs to the same worker stream and includes the
	// union; the local forwarder creates no independent server request source.
	for _, request := range plane.Requests() {
		for _, topic := range machineevents.DaemonTopics {
			if _, ok := request[string(topic)]; !ok {
				t.Fatalf("worker wait missing %s", topic)
			}
		}
	}
}
