//go:build darwin || linux

package runtime

import (
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	clientapi "github.com/pinksaucepasta/paperboat/internal/api"
	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
)

// A machine whose cloned host keys the server refused (Lane H, official 2026.10.23.5) publishes its
// regenerated keys at the next tick, and does not ask again while the refused keys are unchanged.
func TestRefusedHostKeysRepublishOnceRegenerated(t *testing.T) {
	inUse := &clientapi.APIError{Status: http.StatusConflict, Code: managedSSHHostKeyInUseCode}
	key := managedSSHTestPublicKey(t)
	client := &rotatingManagedSSHClient{observeErrors: []error{inUse}, keys: [][]string{{key}}}
	var mu sync.Mutex
	onDisk := []string{"ssh-ed25519 AAAA cloned"}
	read := func() ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		return onDisk, nil
	}
	service := &managedSSHKeyReconciler{client: client, identity: &refreshingManagedSSHIdentity{}, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 9, publicKeys: onDisk, readPublicKeys: read, home: managedSSHRuntimeTestHome(t), ownerUID: uint32(os.Getuid()), interval: 10 * time.Millisecond, timeout: time.Second}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(t.Context())
	time.Sleep(100 * time.Millisecond)
	client.mu.Lock()
	asked := client.hostCalls
	client.mu.Unlock()
	if asked != 1 || service.CapabilityHealth().Reason != managedSSHHostKeyInUseCode {
		t.Fatalf("unchanged refused keys: asked %d times, health %+v", asked, service.CapabilityHealth())
	}
	mu.Lock()
	onDisk = []string{"ssh-ed25519 BBBB regenerated"}
	mu.Unlock()
	deadline := time.Now().Add(2 * time.Second)
	for !service.ready.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("regenerated keys were not published: health %+v", service.CapabilityHealth())
		}
		time.Sleep(5 * time.Millisecond)
	}
}
