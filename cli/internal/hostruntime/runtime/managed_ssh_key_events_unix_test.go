//go:build darwin || linux

package runtime

import (
	"context"
	"os"
	"testing"
	"time"

	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
)

// PR-03: while key changes are delivered, an idle host does not ask for its managed SSH keys on a
// timer; a published machine_access change makes it ask at once.
func TestManagedSSHKeysAreFetchedOnlyWhenAChangeIsPublished(t *testing.T) {
	publish := make(chan struct{})
	generation := int64(1)
	hub, err := machineevents.NewWithWaiter(func(ctx context.Context, known map[machineevents.Topic]int64) (map[machineevents.Topic]int64, error) {
		if known[machineevents.TopicMachineAccess] >= generation {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-publish:
				generation++
			}
		}
		return map[machineevents.Topic]int64{machineevents.TopicMachineAccess: generation}, nil
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	events := hub.Subscribe(machineevents.TopicMachineAccess)
	if err := hub.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer hub.Shutdown(context.Background())

	key := managedSSHTestPublicKey(t)
	client := &rotatingManagedSSHClient{keys: [][]string{{key}}}
	service := &managedSSHKeyReconciler{client: client, identity: &refreshingManagedSSHIdentity{}, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 9, publicKeys: []string{key}, home: managedSSHRuntimeTestHome(t), ownerUID: uint32(os.Getuid()), interval: 10 * time.Millisecond, timeout: time.Second, events: events}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	asked := func() int {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.hostCalls
	}
	time.Sleep(200 * time.Millisecond) // the hub's first answer may wake one refresh
	idle := asked()
	time.Sleep(300 * time.Millisecond) // 30 intervals
	if got := asked(); got != idle {
		t.Fatalf("idle host asked for its keys %d more times on a timer", got-idle)
	}
	publish <- struct{}{}
	deadline := time.Now().Add(5 * time.Second)
	for asked() == idle {
		if time.Now().After(deadline) {
			t.Fatal("a published key change did not refresh the keys")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
