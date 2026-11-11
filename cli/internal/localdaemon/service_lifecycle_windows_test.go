//go:build windows

package localdaemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"golang.org/x/sys/windows"
)

const testServiceOwnerSID = "S-1-5-21-1-2-3-1001"

// fakeWindowsDaemonService replaces the service manager and the elevation bridge. probeErr is what
// opening the service manager returns (access denied in a terminal that is not elevated).
type fakeWindowsDaemonService struct {
	installed bool
	probeErr  error
	started   int
	elevated  []string
}

func installFakeWindowsDaemonService(t *testing.T, fake *fakeWindowsDaemonService) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "pb.exe")
	if err := os.WriteFile(executable, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	previousSID, previousProbe, previousStart, previousElevated := currentWindowsServiceSID, windowsOwnerServiceInstalled, invokeWindowsOwnerServiceStart, runElevatedDaemonService
	t.Cleanup(func() {
		currentWindowsServiceSID, windowsOwnerServiceInstalled, invokeWindowsOwnerServiceStart, runElevatedDaemonService = previousSID, previousProbe, previousStart, previousElevated
	})
	currentWindowsServiceSID = func() (string, error) { return testServiceOwnerSID, nil }
	windowsOwnerServiceInstalled = func(owner string) (bool, error) {
		if owner != testServiceOwnerSID {
			t.Fatalf("owner=%q", owner)
		}
		return fake.installed, fake.probeErr
	}
	invokeWindowsOwnerServiceStart = func(context.Context, string) error { fake.started++; return nil }
	runElevatedDaemonService = func(_ context.Context, helper, action string) error {
		if helper != executable {
			t.Fatalf("elevated helper=%q want the running CLI %q", helper, executable)
		}
		fake.elevated = append(fake.elevated, action)
		// The elevated side is the installer's own code: it registers or removes the service.
		fake.installed = action == elevation.ActionDaemonInstall
		return nil
	}
	return executable
}

func TestCreateCurrentUserServiceRegistersAMissingServiceAsAdministrator(t *testing.T) {
	fake := &fakeWindowsDaemonService{}
	executable := installFakeWindowsDaemonService(t, fake)
	if err := CreateCurrentUserService(context.Background(), executable, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(fake.elevated) != 1 || fake.elevated[0] != elevation.ActionDaemonInstall || fake.started != 0 {
		t.Fatalf("elevated=%q started=%d", fake.elevated, fake.started)
	}
}

func TestCreateCurrentUserServiceStartsAnInstalledServiceWithoutElevation(t *testing.T) {
	fake := &fakeWindowsDaemonService{installed: true}
	executable := installFakeWindowsDaemonService(t, fake)
	for range 2 {
		if err := CreateCurrentUserService(context.Background(), executable, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if len(fake.elevated) != 0 || fake.started != 2 {
		t.Fatalf("elevated=%q started=%d", fake.elevated, fake.started)
	}
}

func TestCreateCurrentUserServiceElevatesWhenTheServiceManagerDeniesAccess(t *testing.T) {
	fake := &fakeWindowsDaemonService{probeErr: fmt.Errorf("open service manager: %w", windows.ERROR_ACCESS_DENIED)}
	executable := installFakeWindowsDaemonService(t, fake)
	if err := CreateCurrentUserService(context.Background(), executable, "", ""); err != nil {
		t.Fatal(err)
	}
	if len(fake.elevated) != 1 || fake.elevated[0] != elevation.ActionDaemonInstall {
		t.Fatalf("elevated=%q", fake.elevated)
	}
}

func TestCreateCurrentUserServiceRefusesLaunchOverridesAndKeepsOtherFailures(t *testing.T) {
	fake := &fakeWindowsDaemonService{}
	executable := installFakeWindowsDaemonService(t, fake)
	if err := CreateCurrentUserService(context.Background(), executable, filepath.Join(t.TempDir(), "config.json"), ""); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("config override error=%v", err)
	}
	if err := CreateCurrentUserService(context.Background(), executable, "", "https://api.example.test"); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("server override error=%v", err)
	}
	failure := errors.New("service manager unavailable")
	fake.probeErr = failure
	if err := CreateCurrentUserService(context.Background(), executable, "", ""); !errors.Is(err, failure) {
		t.Fatalf("error=%v", err)
	}
	if len(fake.elevated) != 0 {
		t.Fatalf("elevated=%q", fake.elevated)
	}
}

func TestCreateCurrentUserServiceReturnsADeniedElevation(t *testing.T) {
	fake := &fakeWindowsDaemonService{}
	executable := installFakeWindowsDaemonService(t, fake)
	runElevatedDaemonService = func(context.Context, string, string) error { return elevation.ErrElevationDenied }
	if err := CreateCurrentUserService(context.Background(), executable, "", ""); !errors.Is(err, elevation.ErrElevationDenied) {
		t.Fatalf("error=%v", err)
	}
}

// A command that only finds no daemon (pb status, pb doctor) never asks for administrator rights.
func TestInstallCurrentUserServiceNeverElevates(t *testing.T) {
	fake := &fakeWindowsDaemonService{}
	executable := installFakeWindowsDaemonService(t, fake)
	err := InstallCurrentUserService(context.Background(), executable, "", "")
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) || !errors.Is(err, ErrServiceNotInstalled) {
		t.Fatalf("error=%v", err)
	}
	if len(fake.elevated) != 0 {
		t.Fatalf("elevated=%q", fake.elevated)
	}
}

func TestUninstallCurrentUserServiceRemovesARunningOrMissingService(t *testing.T) {
	for _, installed := range []bool{true, false} {
		fake := &fakeWindowsDaemonService{installed: installed}
		executable := installFakeWindowsDaemonService(t, fake)
		if err := UninstallCurrentUserService(context.Background(), executable); err != nil {
			t.Fatalf("installed=%t: %v", installed, err)
		}
		if len(fake.elevated) != 1 || fake.elevated[0] != elevation.ActionDaemonRemove {
			t.Fatalf("installed=%t elevated=%q", installed, fake.elevated)
		}
		state, err := InspectCurrentUserService(context.Background(), executable)
		if err != nil || state != (ServiceState{}) {
			t.Fatalf("installed=%t status after uninstall=%+v error=%v", installed, state, err)
		}
		if err := StartCurrentUserService(context.Background(), executable); err != ErrServiceNotInstalled {
			t.Fatalf("installed=%t start after uninstall error=%v", installed, err)
		}
		if err := CreateCurrentUserService(context.Background(), executable, "", ""); err != nil || !fake.installed {
			t.Fatalf("installed=%t reinstall error=%v", installed, err)
		}
	}
}

func TestUninstallCurrentUserServiceRejectsAnInvalidHelper(t *testing.T) {
	fake := &fakeWindowsDaemonService{installed: true}
	installFakeWindowsDaemonService(t, fake)
	if err := UninstallCurrentUserService(context.Background(), filepath.Join(t.TempDir(), "missing.exe")); !errors.Is(err, ErrInvalidInventoryConfig) {
		t.Fatalf("error=%v", err)
	}
	if len(fake.elevated) != 0 {
		t.Fatalf("elevated=%q", fake.elevated)
	}
}

func deniedByServiceManager() error {
	return fmt.Errorf("open service manager: %w", windows.ERROR_ACCESS_DENIED)
}

// An un-elevated terminal's own start, stop and removal are refused by the service manager; each of them
// then asks for administrator rights through the same bridge, with its own action.
func TestServiceActionsElevateWhenTheServiceManagerDeniesAccess(t *testing.T) {
	fake := &fakeWindowsDaemonService{installed: true}
	executable := installFakeWindowsDaemonService(t, fake)
	previousStop, previousStopService := stopCurrentUserDaemon, stopWindowsLocalDaemonService
	previousTask, previousRead := runWindowsTaskCommand, readWindowsDaemonPIDLock
	t.Cleanup(func() {
		stopCurrentUserDaemon, stopWindowsLocalDaemonService = previousStop, previousStopService
		runWindowsTaskCommand, readWindowsDaemonPIDLock = previousTask, previousRead
	})
	invokeWindowsOwnerServiceStart = func(context.Context, string) error { return deniedByServiceManager() }
	stopCurrentUserDaemon = func(context.Context) error { return deniedByServiceManager() }
	stopWindowsLocalDaemonService = func(context.Context) error { return deniedByServiceManager() }
	runWindowsTaskCommand = func(context.Context, ...string) error {
		return &windowsTaskCommandError{err: errors.New("exit status 1"), kind: windowsTaskErrorMissing}
	}
	readWindowsDaemonPIDLock = func(string, string) (windowsDaemonPIDLock, error) { return windowsDaemonPIDLock{}, os.ErrNotExist }
	for _, step := range []struct {
		name   string
		run    func() error
		action string
	}{
		{"start", func() error { return StartCurrentUserService(context.Background(), executable) }, elevation.ActionDaemonStart},
		{"stop", func() error { return StopCurrentUserService(context.Background(), executable) }, elevation.ActionDaemonStop},
		{"uninstall step", func() error { return RemoveCurrentUserService(context.Background(), executable) }, elevation.ActionDaemonRemove},
	} {
		fake.elevated = nil
		if err := step.run(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if len(fake.elevated) != 1 || fake.elevated[0] != step.action {
			t.Fatalf("%s: elevated=%q want %q", step.name, fake.elevated, step.action)
		}
	}
}

func TestServiceActionsDoNotElevateForOtherFailures(t *testing.T) {
	fake := &fakeWindowsDaemonService{installed: true}
	executable := installFakeWindowsDaemonService(t, fake)
	previous := stopCurrentUserDaemon
	t.Cleanup(func() { stopCurrentUserDaemon = previous })
	failure := errors.New("service is stuck")
	stopCurrentUserDaemon = func(context.Context) error { return failure }
	if err := StopCurrentUserService(context.Background(), executable); !errors.Is(err, failure) || len(fake.elevated) != 0 {
		t.Fatalf("error=%v elevated=%q", err, fake.elevated)
	}
}

// A declined or impossible prompt ends in the typed error that tells the user how to get administrator
// rights, from every action, and still matches the bridge's own error.
func TestServiceActionsReportDeclinedElevationAsAdministratorRequired(t *testing.T) {
	for _, cause := range []error{elevation.ErrElevationDenied, elevation.ErrElevationUnavailable, elevation.ErrElevationRestrictedJob} {
		fake := &fakeWindowsDaemonService{installed: true}
		executable := installFakeWindowsDaemonService(t, fake)
		previous := stopCurrentUserDaemon
		t.Cleanup(func() { stopCurrentUserDaemon = previous })
		stopCurrentUserDaemon = func(context.Context) error { return deniedByServiceManager() }
		runElevatedDaemonService = func(context.Context, string, string) error { return cause }
		err := StopCurrentUserService(context.Background(), executable)
		var needed *AdministratorRequiredError
		if !errors.As(err, &needed) || !errors.Is(err, cause) {
			t.Fatalf("cause=%v error=%v", cause, err)
		}
		if cause == elevation.ErrElevationRestrictedJob {
			if needed.Error() != cause.Error() {
				t.Fatalf("restricted job message=%q", needed.Error())
			}
		} else if !strings.Contains(needed.Error(), "administrator") || !strings.Contains(needed.Error(), "Windows prompt") {
			t.Fatalf("message=%q", needed.Error())
		}
	}
	// pb service install and uninstall use the same path.
	fake := &fakeWindowsDaemonService{}
	executable := installFakeWindowsDaemonService(t, fake)
	runElevatedDaemonService = func(context.Context, string, string) error { return elevation.ErrElevationDenied }
	var needed *AdministratorRequiredError
	if err := CreateCurrentUserService(context.Background(), executable, "", ""); !errors.As(err, &needed) {
		t.Fatalf("install error=%v", err)
	}
	if err := UninstallCurrentUserService(context.Background(), executable); !errors.As(err, &needed) {
		t.Fatalf("uninstall error=%v", err)
	}
}
