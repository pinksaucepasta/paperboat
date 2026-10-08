//go:build windows

package updated

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/bootstrap"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

// Real PE bytes and protected temporary files exercise the startup identity
// gate. No SCM service, enrolled installation, listener or callback is replaced.
func TestNativeWindowsFeatureSourceCommitRollbackAndStartup(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	if user.User.Sid.String() != "S-1-5-18" {
		t.Skip("requires native SYSTEM protected-file fixture")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	previousBody, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	// This test executable is unsigned. A PE overlay changes its digest while
	// preserving a real valid executable, without running either fixture.
	candidateBody := append(append([]byte(nil), previousBody...), 0x42)
	for _, maintenance := range []bool{false, true} {
		for _, distribution := range []string{installsource.Custom, installsource.Official} {
			name := distribution + "/ordinary"
			if maintenance {
				name = distribution + "/maintenance"
			}
			t.Run(name, func(t *testing.T) {
				root := t.TempDir()
				ownerSID := "S-1-5-21-101-202-303-1001"
				layout, err := service.WindowsUserLayout(ownerSID)
				if err != nil {
					t.Fatal(err)
				}
				binary := filepath.Join(root, "pb.exe")
				writeProtected := func(path string, body []byte, dacl string) {
					t.Helper()
					if err := os.WriteFile(path, body, 0600); err != nil {
						t.Fatal(err)
					}
					if err := applyWindowsReleaseACL(path, dacl); err != nil {
						t.Fatal(err)
					}
				}
				writeProtected(binary, candidateBody, windowsStableBinaryDACL(ownerSID))
				identity := func(body []byte, version, distribution string) installsource.Source {
					digest := sha256.Sum256(body)
					return installsource.Source{Version: version, Platform: "windows", Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(digest[:]), Length: int64(len(body)), Distribution: distribution, AutomaticUpdates: distribution == installsource.Official}
				}
				previous := identity(previousBody, "2026.10.08.20", distribution)
				candidate := identity(candidateBody, "2026.10.08.31", installsource.Official)
				installPath := filepath.Join(root, "runtime-install.json")
				document := hostinstall.WindowsRuntimeConfig{Schema: "paperboat.windows-runtime-install/v1", Instance: layout.Instance, OwnerSID: ownerSID, User: "native-test", MachineID: "machine-test", Source: previous, Committed: true, Artifact: bootstrap.ArtifactTarget{Schema: bootstrap.ArtifactTargetSchemaV1, Kind: bootstrap.ArtifactKindPB, Version: previous.Version, Platform: "windows", Architecture: runtime.GOARCH, RepositoryURL: "https://example.invalid/tuf", TargetPath: "pb-windows-" + runtime.GOARCH + ".exe"}}
				body, err := json.Marshal(document)
				if err != nil {
					t.Fatal(err)
				}
				writeProtected(installPath, body, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FR;;;"+ownerSID+")")
				stateRoot := filepath.Join(root, "updated")
				if err := os.MkdirAll(filepath.Join(stateRoot, "activation"), 0700); err != nil {
					t.Fatal(err)
				}
				config := WindowsConfig{OwnerSID: ownerSID, StateRoot: stateRoot, Binary: binary, InstallState: installPath, Source: previous, RepositoryURL: document.Artifact.RepositoryURL, Architecture: runtime.GOARCH}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				if _, err := WindowsFeatureVersion(ctx, config); !errors.Is(err, installsource.ErrInvalid) {
					t.Fatalf("old Source accepted new bytes: %v", err)
				}
				j := testWindowsActivationJournal()
				j.Version, j.PreviousVersion, j.Architecture = candidate.Version, previous.Version, runtime.GOARCH
				j.Release.Version, j.Release.Architecture, j.Release.SupervisorMaintenance = candidate.Version, runtime.GOARCH, maintenance
				j.Release.SHA256, j.Release.Length = candidate.SHA256, candidate.Length
				j.Candidate.Version, j.Candidate.Architecture = candidate.Version, runtime.GOARCH
				j.Candidate.SHA256, j.Candidate.Length = candidate.SHA256, candidate.Length
				paths, err := canonicalWindowsRelease(layout, j.Version)
				if err != nil {
					t.Fatal(err)
				}
				oldPaths, err := canonicalWindowsRelease(layout, j.PreviousVersion)
				if err != nil {
					t.Fatal(err)
				}
				component := windowsActivationComponent{Path: paths.Runtime, SHA256: candidate.SHA256, Length: candidate.Length}
				j.Runtime, j.CLI, j.Hostd, j.Updater = component, component, component, component
				j.PreviousBinary = windowsActivationComponent{Path: layout.Binary, SHA256: previous.SHA256, Length: previous.Length}
				j.PreviousSource = &previous
				j.PreviousRuntime = j.PreviousBinary
				j.PreviousRuntime.Path = oldPaths.Runtime
				j.OldHostd = windowsServiceTarget{Executable: oldPaths.Runtime, SHA256: previous.SHA256, Length: previous.Length, Arguments: []string{"daemon", "__runtime-hostd", "--instance", layout.Instance}}
				j.OldUpdater = windowsServiceTarget{Executable: oldPaths.Runtime, SHA256: previous.SHA256, Length: previous.Length, Arguments: []string{"daemon", "__runtime-updated", "--instance", layout.Instance}}
				j.NewHostd, j.NewUpdater = j.OldHostd, j.OldUpdater
				if maintenance {
					j.NewHostd.Executable, j.NewUpdater.Executable = paths.Runtime, paths.Runtime
				}
				j.Stage = windowsActivationServicesLive
				bindWindowsTestCandidate(&j)
				backend := newWindowsSCMActivationBackend(config)
				if err := backend.WriteJournal(j); err != nil {
					t.Fatal(err)
				}
				if got, err := WindowsFeatureVersion(ctx, config); err != nil || got != candidate.Version {
					t.Fatalf("protected pending candidate startup=%q error=%v", got, err)
				}
				for range 2 {
					if err := backend.CommitCLI(ctx, j); err != nil {
						t.Fatal(err)
					}
				}
				readDocument := func() hostinstall.WindowsRuntimeConfig {
					t.Helper()
					body, err := os.ReadFile(installPath)
					if err != nil {
						t.Fatal(err)
					}
					var got hostinstall.WindowsRuntimeConfig
					if err := json.Unmarshal(body, &got); err != nil {
						t.Fatal(err)
					}
					return got
				}
				got := readDocument()
				candidate.AutomaticUpdates = previous.AutomaticUpdates
				if got.Source != candidate || got.Artifact.Version != candidate.Version || got.Artifact.Platform != candidate.Platform || got.Artifact.Architecture != candidate.Architecture || got.User != document.User || got.MachineID != document.MachineID || got.Artifact.RepositoryURL != document.Artifact.RepositoryURL || got.Artifact.TargetPath != document.Artifact.TargetPath {
					t.Fatal("commit lost exact identity, enrollment or update policy")
				}
				if err := os.Remove(windowsActivationJournalPath(stateRoot)); err != nil {
					t.Fatal(err)
				}
				config.Source = got.Source
				if version, err := WindowsFeatureVersion(ctx, config); err != nil || version != candidate.Version {
					t.Fatalf("committed Source startup=%q error=%v", version, err)
				}
				writeProtected(binary, previousBody, windowsStableBinaryDACL(ownerSID))
				j.Stage = windowsActivationRollingBack
				if err := backend.WriteJournal(j); err != nil {
					t.Fatal(err)
				}
				if version, err := WindowsFeatureVersion(ctx, config); err != nil || version != previous.Version {
					t.Fatalf("pending restored Source startup=%q error=%v", version, err)
				}
				for range 2 {
					if err := backend.CommitCLI(ctx, j); err != nil {
						t.Fatal(err)
					}
				}
				got = readDocument()
				if got.Source != previous || got.Artifact.Version != previous.Version {
					t.Fatal("rollback lost exact PreviousSource provenance")
				}
				// Neither Source transition may rewrite either native role pin.
				if j.OldUpdater.Executable != oldPaths.Runtime || j.OldHostd.Executable != oldPaths.Runtime {
					t.Fatal("native pins changed")
				}
				original, err := os.ReadFile(installPath)
				if err != nil {
					t.Fatal(err)
				}
				writeProtected(binary, candidateBody, windowsStableBinaryDACL(ownerSID))
				if err := backend.CommitCLI(ctx, j); err == nil {
					t.Fatal("rollback accepted wrong canonical bytes")
				}
				after, err := os.ReadFile(installPath)
				if err != nil || string(after) != string(original) {
					t.Fatal("wrong-byte rejection changed protected Source")
				}
				// A legal journal cannot authorize a writable canonical file.
				writeProtected(binary, previousBody, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;"+ownerSID+")")
				if _, err := WindowsFeatureVersion(ctx, config); err == nil {
					t.Fatal("startup accepted writable canonical bytes")
				}
				writeProtected(binary, previousBody, windowsStableBinaryDACL(ownerSID))
				if err := applyWindowsReleaseACL(windowsActivationJournalPath(stateRoot), "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;"+ownerSID+")"); err != nil {
					t.Fatal(err)
				}
				if _, err := WindowsFeatureVersion(ctx, config); err == nil {
					t.Fatal("startup accepted unprotected journal")
				}
			})
		}
	}
}
