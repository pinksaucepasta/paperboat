//go:build windows

package updated

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"golang.org/x/sys/windows"
)

// Exercise the actual protected enrolled installation and its real owner pipe.
func TestNativeWindowsSystemOwnerReadiness(t *testing.T) {
	instance := os.Getenv("PAPERBOAT_NATIVE_OWNER_INSTANCE")
	if instance == "" {
		t.Skip("requires enrolled native Windows fixture")
	}
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	config, err := hostinstall.LoadWindowsRuntimeConfigForInstance(instance)
	if err != nil {
		t.Fatal("protected owner config unavailable")
	}
	layout, err := service.WindowsUserLayout(config.OwnerSID)
	if err != nil || layout.Instance != instance {
		t.Fatal("owner layout mismatch")
	}
	paths, err := localapi.WindowsPaths(config.StateRoot, config.OwnerSID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if token.User.Sid.String() != "S-1-5-18" {
		if _, err := localapi.ReadSystemOwnerSnapshot(ctx, paths.SocketPath, config.OwnerSID, time.Second); err == nil {
			t.Fatal("ordinary user obtained system-only probe")
		}
		return
	}
	snapshot, err := localapi.ReadSystemOwnerSnapshot(ctx, paths.SocketPath, config.OwnerSID, 2*time.Second)
	if err != nil || snapshot.DaemonVersion != config.Source.Version || snapshot.DaemonState != "ready" {
		t.Fatalf("readiness version=%s state=%s err=%v", snapshot.DaemonVersion, snapshot.DaemonState, err)
	}
	if _, err := localapi.ReadSystemOwnerSnapshot(ctx, paths.SocketPath, "S-1-5-18", time.Second); err == nil {
		t.Fatal("foreign owner pipe accepted")
	}
	installPath, err := hostinstall.WindowsInstanceConfigPath(instance)
	if err != nil {
		t.Fatal(err)
	}
	cfg := WindowsConfig{Source: config.Source, StateRoot: layout.UpdateStateRoot, RuntimeStateRoot: config.StateRoot, Binary: layout.Binary, BinaryRollback: layout.BinaryRollback, BinaryStaged: layout.BinaryStaged, OwnerSID: config.OwnerSID, MachineID: config.MachineID, RepositoryURL: config.Artifact.RepositoryURL, TokenFile: config.TokenFile, InstallState: installPath, ControlSocket: layout.UpdaterSocket, HostdSocket: layout.HostdSocket, HealthURL: "http://" + config.ListenAddress + "/healthz", ActiveVersion: config.Source.Version, Architecture: config.Artifact.Architecture, SetupMode: config.SetupMode}
	backend := newWindowsSCMActivationBackend(cfg)
	journal, err := loadWindowsActivationJournal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.AuthorizeRecovery(ctx, journal); err != nil {
		t.Fatalf("recovery authorization: %v", err)
	}
	if err := backend.VerifyRollback(ctx, journal); err != nil {
		t.Fatalf("rollback readiness: %v", err)
	}
	t.Log("SYSTEM owner snapshot, foreign-owner denial, recovery authorization and rollback readiness passed")
}
