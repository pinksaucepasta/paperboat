//go:build darwin || linux

package runtime

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
)

func TestProtectConfigSyncRuntimeStateExcludesStateInsideManagedHome(t *testing.T) {
	descriptor, err := protectConfigSyncRuntimeState(
		configsync.RuntimeDescriptor{},
		"/Users/sailor",
		"/Users/sailor/Library/Application Support/paperboat/helper[state]",
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := descriptor.Policy.RuntimeExclusionRoots; len(got) != 1 || got[0] != "Library/Application Support/paperboat/helper[state]" {
		t.Fatalf("runtime exclusions = %#v", got)
	}
}

func TestProtectConfigSyncRuntimeStateLeavesExternalStateOutsideManagedHome(t *testing.T) {
	descriptor, err := protectConfigSyncRuntimeState(
		configsync.RuntimeDescriptor{}, "/Users/sailor", "/var/lib/paperboat/helper",
	)
	if err != nil || len(descriptor.Policy.RuntimeExclusionRoots) != 0 {
		t.Fatalf("descriptor = %#v, error = %v", descriptor, err)
	}
}

func TestProtectConfigSyncRuntimeStateRejectsManagedHome(t *testing.T) {
	if _, err := protectConfigSyncRuntimeState(
		configsync.RuntimeDescriptor{}, "/Users/sailor", "/Users/sailor",
	); !errors.Is(err, ErrProductionInvalid) {
		t.Fatalf("state root equal to home error = %v", err)
	}
}

func TestStandaloneConfigWorkerWaitsForCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		finished <- RunProductionConfigWorker(ctx, ProductionConfigWorkerConfig{
			ControlURL: "https://api.example.invalid", StateRoot: filepath.Join(t.TempDir(), "state"),
			HomeRoot: t.TempDir(), RepositoryHosts: []string{"github.com"},
		})
	}()
	select {
	case err := <-finished:
		t.Fatalf("standalone worker exited while still authorized to run: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("worker did not shut down cleanly: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("standalone worker did not join its supervisor after cancellation")
	}
}
