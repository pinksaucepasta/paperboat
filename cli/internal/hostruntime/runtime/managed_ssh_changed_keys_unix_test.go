//go:build darwin || linux

package runtime

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
)

// C066: the owner regenerates the machine's SSH host keys while change delivery is healthy (no event
// comes for a local change). The host reports the new keys within an interval, so the server holds them
// as pending and `pb ssh` can name `pb ssh trust-host --fingerprint`; it does not wait for the fallback.
func TestChangedHostKeysAreReportedWithinAnInterval(t *testing.T) {
	hub, err := machineevents.NewWithWaiter(func(ctx context.Context, known map[machineevents.Topic]int64) (map[machineevents.Topic]int64, error) {
		if known[machineevents.TopicMachineAccess] >= 1 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return map[machineevents.Topic]int64{machineevents.TopicMachineAccess: 1}, nil
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
	var mu sync.Mutex
	onDisk := []string{"ssh-ed25519 AAAA original"}
	read := func() ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return onDisk, nil
	}
	service := &managedSSHKeyReconciler{client: client, identity: &refreshingManagedSSHIdentity{}, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 9, publicKeys: onDisk, readPublicKeys: read, home: managedSSHRuntimeTestHome(t), ownerUID: uint32(os.Getuid()), interval: 10 * time.Millisecond, timeout: time.Second, events: events}
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
	time.Sleep(100 * time.Millisecond)
	if got := asked(); got != idle {
		t.Fatalf("unchanged keys: the idle host asked %d more times", got-idle)
	}
	mu.Lock()
	onDisk = []string{"ssh-ed25519 BBBB regenerated"}
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for asked() == idle {
		if time.Now().After(deadline) {
			t.Fatal("regenerated host keys were not reported to the server")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
