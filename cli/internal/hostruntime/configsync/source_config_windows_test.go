//go:build windows

package configsync

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWindowsSourceAndExplicitDestinationAcceptStoredCaseSpelling(t *testing.T) {
	root := resolvedTempDir(t)
	stored := filepath.Join(root, "paperboat")
	if err := os.Mkdir(stored, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stored, "config-sync.toml"), []byte("# machine exceptions\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSourceConfig(filepath.Join(root, "Paperboat", "config-sync.toml"), DefaultSourceConfigLimits()); err != nil {
		t.Fatalf("native case alias rejected safe source: %v", err)
	}
	t.Setenv("APPDATA", root)
	source, err := ParseSourceConfig([]byte("[paths.myapp]\nkind = 'directory'\ninclude = ['settings.json', 'keybindings.json']\nexclude = ['**/*.tmp']\n[os.windows.paths.myapp]\nlocal_path = '%APPDATA%/Paperboat/mapped'\n"), DefaultSourceConfigLimits())
	if err != nil {
		t.Fatal(err)
	}
	effective, err := MergeSourceConfigsForOS(source, SourceConfig{}, "windows")
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := ResolveEffectiveConfig(root, effective)
	if err != nil {
		t.Fatalf("explicit known-folder case alias rejected: %v", err)
	}
	got, ok := mapping.LocalPath("myapp/settings.json")
	if !ok || got != filepath.Join(root, "Paperboat", "mapped", "settings.json") {
		t.Fatalf("explicit destination changed: %q, %v", got, ok)
	}
}

func TestWindowsRuntimeConstructorsAcceptStoredCaseSpelling(t *testing.T) {
	root := resolvedTempDir(t)
	for _, name := range []string{"home", "state"} {
		if err := os.Mkdir(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	descriptor := RuntimeDescriptor{WriteMode: "leased_writes", Mode: ModePullOnly, RepositoryID: "repository", AssignmentID: "assignment", AssignmentVersion: 1, EnvironmentID: "environment", MachineID: "helper", WarningRevision: "warning", InstallationGeneration: 1,
		Policy: RuntimePolicy{Format: "paperboat-config-plaintext-v1", Revision: "policy", ManifestContract: ManifestContractVersion, ManifestMaxBytes: DefaultManifestMaxBytes, ManifestMaxLines: DefaultManifestMaxLines, ManifestMaxPatternBytes: DefaultManifestMaxPatternBytes, MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, Debounce: time.Second, MinimumPushInterval: time.Minute, MaximumDirtyDelay: time.Minute, RemotePollInterval: time.Hour, RetryLimit: 1, ShutdownFlushTimeout: time.Second, SummaryLimit: 10}}
	home, state := filepath.Join(root, "Home"), filepath.Join(root, "State")
	if _, err := NewPlaintextWorkspaceReconciler(WorkspaceReconcilerConfig{HomeRoot: home, StateRoot: state, Descriptor: descriptor}); err != nil {
		t.Fatalf("safe stored-case roots rejected: %v", err)
	}
	if _, err := NewEngine(EngineConfig{HomeRoot: home, Descriptor: descriptor, Syncer: failingSyncer{}}); err != nil {
		t.Fatalf("safe stored-case home rejected: %v", err)
	}
}

func TestWindowsNewSourceHasStableUserAccessAndNeverOverwrites(t *testing.T) {
	root := resolvedTempDir(t)
	path := filepath.Join(root, "config-sync.toml")
	file, err := CreateSourceFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("# owner configuration\n"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	want, err := currentPrivateFileDescriptor()
	if err != nil {
		t.Fatal(err)
	}
	if !windowssecurity.OwnerMatchesSID(path, user.User.Sid) || !windowssecurity.ProtectedDACLMatches(path, want.String()) {
		t.Fatal("new source is not owned by and readable to the stable user SID")
	}
	if _, err = LoadSourceConfig(path, DefaultSourceConfigLimits()); err != nil {
		t.Fatal(err)
	}
	if file, err = CreateSourceFile(path); !errors.Is(err, os.ErrExist) {
		if file != nil {
			file.Close()
		}
		t.Fatalf("existing source accepted: %v", err)
	}
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != "# owner configuration\n" {
		t.Fatal("existing source changed")
	}
}
