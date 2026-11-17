package daemoncmd

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/machineevents"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
)

func TestPairedDaemonUsesWorkerPushWithoutServerWait(t *testing.T) {
	root := t.TempDir()
	t.Setenv("PAPERBOAT_RUNTIME_STATE_ROOT", root)
	if daemonHasWorker() {
		t.Fatal("worker exists before installation")
	}
	if err := os.MkdirAll(filepath.Join(root, "runtime"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "runtime", "worker-local.json"), []byte(`{"schema":"paperboat.worker-local/v1","listen_address":"127.0.0.1:12345"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var waits atomic.Int32
	events, err := newDaemonMachineEvents("machine_test", daemonHasWorker(), func(ctx context.Context, _ map[machineevents.Topic]int64) (map[machineevents.Topic]int64, error) {
		waits.Add(1)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	subscriptions := make([]*machineevents.Subscription, 0, len(machineevents.DaemonTopics))
	for _, topic := range machineevents.DaemonTopics {
		subscriptions = append(subscriptions, events.hub.Subscribe(topic))
	}
	if err := events.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer events.stop(context.Background())
	if events.remote {
		t.Fatal("paired daemon constructed another server hub")
	}
	for i, topic := range machineevents.DaemonTopics {
		// Unhealthy stream still delivers invalidations, without recovery waking unrelated topics.
		if err := events.receive(t.Context(), localapi.MachineEventPush{MachineID: "machine_test", Topics: []machineevents.Topic{topic}}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-subscriptions[i].Changed():
		case <-time.After(time.Second):
			t.Fatalf("%s consumer missed push", topic)
		}
	}
	if waits.Load() != 0 {
		t.Fatalf("paired daemon made %d extra waits", waits.Load())
	}
	if err := events.receive(t.Context(), localapi.MachineEventPush{MachineID: "other_machine", Topics: []machineevents.Topic{machineevents.TopicInventory}}); err == nil {
		t.Fatal("another machine invalidated daemon")
	}
}

func TestDaemonWithoutWorkerWaitStopsOnFirstWorkerPush(t *testing.T) {
	entered, ended := make(chan struct{}), make(chan struct{})
	events, err := newDaemonMachineEvents("machine_test", false, func(ctx context.Context, _ map[machineevents.Topic]int64) (map[machineevents.Topic]int64, error) {
		close(entered)
		<-ctx.Done()
		close(ended)
		return nil, ctx.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	sub := events.hub.Subscribe(machineevents.TopicInventory)
	if err := events.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer events.stop(context.Background())
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("user-credential fallback never waited")
	}
	if err := events.receive(t.Context(), localapi.MachineEventPush{MachineID: "machine_test", Topics: []machineevents.Topic{machineevents.TopicInventory}, Healthy: true}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	default:
		t.Fatal("worker push left credential wait running")
	}
	select {
	case <-sub.Changed():
	default:
		t.Fatal("worker takeover missed consumer")
	}
	if events.remote {
		t.Fatal("fallback retained after worker takeover")
	}
	unpaired, err := newDaemonMachineEvents("", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := unpaired.start(t.Context()); err != nil {
		t.Fatal(err)
	}
	if unpaired.remote {
		t.Fatal("unenrolled daemon must poll without a machine id")
	}
}
