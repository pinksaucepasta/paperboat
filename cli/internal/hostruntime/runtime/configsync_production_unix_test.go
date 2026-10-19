//go:build darwin || linux

package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/configsync"
)

func TestProductionConfigRepositoryDefersMappedIOAndPreservesOutputObservation(t *testing.T) {
	home, state, credentials := t.TempDir(), t.TempDir(), t.TempDir()
	// An unapproved destination must not be inspected while assembling the
	// runtime. The reconciler checks the actual files before resolving it.
	destination := filepath.Join(home, "settings")
	if err := os.Symlink(filepath.Join(home, "unapproved-target"), destination); err != nil {
		t.Fatal(err)
	}
	descriptor := configsync.RuntimeDescriptor{
		WriteMode: "leased_writes", Mode: configsync.ModeBidirectional,
		RepositoryID: "repository", PullRepositoryID: "pull", PushRepositoryID: "push",
		AssignmentID: "assignment", AssignmentVersion: 1, EnvironmentID: "environment",
		MachineID: "helper", InstallationGeneration: 1, WarningRevision: "warning",
		PathRules: []configsync.PathRule{{ID: "machine:settings", Source: "machine", RepositoryPath: "settings", LocalPath: destination, Kind: "file"}},
		Policy: configsync.RuntimePolicy{
			Format: "paperboat-config-plaintext-v1", Revision: "policy",
			ManifestContract: configsync.ManifestContractVersion, ManifestMaxBytes: configsync.DefaultManifestMaxBytes,
			ManifestMaxLines: configsync.DefaultManifestMaxLines, ManifestMaxPatternBytes: configsync.DefaultManifestMaxPatternBytes,
			MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, Debounce: time.Second,
			MinimumPushInterval: time.Minute, MaximumDirtyDelay: time.Minute, RemotePollInterval: time.Hour,
			RetryLimit: 1, ShutdownFlushTimeout: time.Second, SummaryLimit: 10,
		},
	}
	repository, reconciler, err := productionConfigRepository(home, state, credentials, descriptor, configsync.SourceConfig{}, nil)
	if err != nil {
		t.Fatalf("production assembly inspected unapproved destination: %v", err)
	}
	if _, ok := repository.(*configsync.SplitRepository); !ok {
		t.Fatal("bidirectional production did not assemble both directions")
	}
	observer, ok := reconciler.(interface {
		ObservedLocalFile(string) (configsync.FileState, bool)
	})
	if !ok {
		t.Fatal("combined production diagnostics dropped output observation")
	}
	if _, known := observer.ObservedLocalFile("settings"); known {
		t.Fatal("production reported local observation before approved reconciliation")
	}
	if _, err := os.Lstat(filepath.Join(home, "unapproved-target")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("production assembly touched unapproved destination")
	}
}

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
	if err != nil || len(descriptor.Policy.RuntimeExclusionRoots) != 0 || len(descriptor.Policy.AbsoluteRuntimeExclusionRoots) != 1 || descriptor.Policy.AbsoluteRuntimeExclusionRoots[0] != "/var/lib/paperboat/helper" {
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

func TestPruneOrphanedAssignmentStateKeepsCurrentAndCredentials(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, strings.Repeat("a", 32))
	orphan := filepath.Join(root, strings.Repeat("b", 32))
	credentials := filepath.Join(root, "repository-credentials")
	for _, dir := range []string{keep, orphan, credentials} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(orphan, "baseline.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := pruneOrphanedAssignmentState(root, keep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphaned assignment state remained: %v", err)
	}
	for _, dir := range []string{keep, credentials} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s was removed: %v", dir, err)
		}
	}
}
