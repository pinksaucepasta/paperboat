//go:build darwin || linux

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	runtimeidentity "github.com/pinksaucepasta/paperboat/internal/hostruntime/identity"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/machineevents"
)

func signalled(t *testing.T, signals <-chan struct{}, within time.Duration) bool {
	t.Helper()
	select {
	case _, open := <-signals:
		return open
	case <-time.After(within):
		return false
	}
}

// Regenerating the keys (delete, then create one file after another) is one signal after the burst; files that
// are not host public keys signal nothing; the signal comes within the quiet period, not at a poll interval.
func TestHostKeyWatchSignalsOncePerBurstOfKeyChanges(t *testing.T) {
	dir := t.TempDir()
	signals, err := watchHostKeyFiles(t.Context(), dir, 100*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sshd_config"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh_host_rsa_key"), []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	if signalled(t, signals, 300*time.Millisecond) {
		t.Fatal("a file that is not a host public key signalled")
	}
	start := time.Now()
	for _, name := range []string{"ssh_host_rsa_key.pub", "ssh_host_ecdsa_key.pub", "ssh_host_ed25519_key.pub"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("key"), 0o644); err != nil {
			t.Fatal(err)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !signalled(t, signals, 2*time.Second) {
		t.Fatal("a regenerated key set did not signal")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the signal took %v", elapsed)
	}
	if signalled(t, signals, 400*time.Millisecond) {
		t.Fatal("one burst signalled twice")
	}
	if err := os.Remove(filepath.Join(dir, "ssh_host_ed25519_key.pub")); err != nil {
		t.Fatal(err)
	}
	if !signalled(t, signals, 2*time.Second) {
		t.Fatal("a removed key did not signal")
	}
}

// A directory that cannot be watched is an error (the reconciler then polls); the watch ending closes the channel.
func TestHostKeyWatchFailsOnAMissingDirectoryAndClosesWhenItEnds(t *testing.T) {
	if _, err := watchHostKeyFiles(t.Context(), filepath.Join(t.TempDir(), "missing"), time.Second); err == nil {
		t.Fatal("a missing directory was watched")
	}
	ctx, cancel := context.WithCancel(t.Context())
	signals, err := watchHostKeyFiles(ctx, t.TempDir(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case _, open := <-signals:
		if open {
			t.Fatal("a signal came with no change")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the channel stayed open after the context ended")
	}
}

// With a watch open the host reads the key files only when the watch signals: a regenerated set is accepted by
// the server at the signal although the interval is an hour, and the idle host does not read at every wake-up.
func TestChangedHostKeysAreReportedAtTheWatchSignal(t *testing.T) {
	hub, err := machineevents.NewWithWaiter(func(ctx context.Context, known map[machineevents.Topic]int64) (map[machineevents.Topic]int64, error) {
		<-ctx.Done()
		return nil, ctx.Err()
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
	reads := 0
	read := func() ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		reads++
		return onDisk, nil
	}
	changes := make(chan struct{}, 1)
	service := &managedSSHKeyReconciler{client: client, identity: &refreshingManagedSSHIdentity{}, registration: runtimeidentity.Registration{MachineID: "machine_1", InstallationGeneration: 4}, workerGeneration: 9, publicKeys: onDisk, readPublicKeys: read, watchHostKeys: func(context.Context) (<-chan struct{}, error) { return changes, nil }, home: managedSSHRuntimeTestHome(t), ownerUID: uint32(os.Getuid()), interval: 20 * time.Millisecond, timeout: time.Second, events: events}
	if err := service.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer service.Shutdown(context.Background())
	time.Sleep(200 * time.Millisecond) // ten wake-ups
	mu.Lock()
	idleReads := reads
	onDisk = []string{"ssh-ed25519 BBBB regenerated"}
	mu.Unlock()
	if idleReads != 0 {
		t.Fatalf("the idle host read the key files %d times with a watch open", idleReads)
	}
	changes <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for {
		client.mu.Lock()
		got := false
		for _, keys := range client.observed {
			got = got || keys == "ssh-ed25519 BBBB regenerated"
		}
		client.mu.Unlock()
		if got {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the regenerated host keys were not reported at the watch signal")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
