//go:build windows

package updated

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"github.com/Microsoft/go-winio"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	nativeSCMCleanupGate        = "PB_NATIVE_WINDOWS223"
	nativeSCMCleanupDescription = "Paperboat task 223 temporary SCM test fixture"
	nativeSCMCleanupRootPrefix  = "PaperboatTask223-updated-"
)

var (
	nativeSCMCleanupRoleFlag = flag.String("paperboat-task223-scm-cleanup-role", "", "task 223 SCM fixture role")
	nativeSCMCleanupNameFlag = flag.String("paperboat-task223-scm-cleanup-name", "", "task 223 SCM fixture name")
	nativeSCMCleanupPipeFlag = flag.String("paperboat-task223-scm-cleanup-pipe", "", "task 223 control pipe endpoint")
)

// TestNativeWindowsSCMCleanupFixtureProcess is the task-only SCM executable
// entry. Before publishing Running, LocalSystem proves the activator endpoint
// was closed and released by the production StartServices path.
func TestNativeWindowsSCMCleanupFixtureProcess(t *testing.T) {
	if *nativeSCMCleanupRoleFlag == "" {
		t.Skip("SCM fixture child only")
	}
	if *nativeSCMCleanupRoleFlag != "updater" || *nativeSCMCleanupNameFlag == "" || *nativeSCMCleanupPipeFlag == "" {
		t.Fatal("invalid task 223 SCM fixture arguments")
	}
	assertNativeSCMCleanupPipeClosed(t, *nativeSCMCleanupPipeFlag)
	if err := svc.Run(*nativeSCMCleanupNameFlag, nativeSCMCleanupService{}); err != nil {
		t.Fatalf("run task 223 SCM fixture: %v", err)
	}
}

type nativeSCMCleanupService struct{}

func (nativeSCMCleanupService) Execute(_ []string, requests <-chan svc.ChangeRequest, statuses chan<- svc.Status) (bool, uint32) {
	statuses <- svc.Status{State: svc.StartPending}
	statuses <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for request := range requests {
		switch request.Cmd {
		case svc.Interrogate:
			statuses <- request.CurrentStatus
		case svc.Stop, svc.Shutdown:
			statuses <- svc.Status{State: svc.StopPending}
			return false, 0
		}
	}
	return false, 0
}

func TestSecureWindowsReleaseFilePreservesMissingFileCause(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing-release.exe")
	err := secureWindowsReleaseFile(missing, "S-1-5-21-111-222-333-1001")
	if !errors.Is(err, os.ErrNotExist) || !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("missing release cause was not preserved: %v", err)
	}
	if got, want := err.Error(), "Windows activation filesystem operation failed"; got != want {
		t.Fatalf("filesystem diagnostic=%q, want static prose %q", got, want)
	}
}

func requireNativeSCMCleanupWindows(t *testing.T) {
	t.Helper()
	if os.Getenv(nativeSCMCleanupGate) != "1" {
		t.Skip("set PB_NATIVE_WINDOWS223=1 in the isolated elevated Windows test session")
	}
}

func nativeSCMCleanupRoot(t *testing.T) (string, *bool) {
	t.Helper()
	base := os.Getenv("ProgramData")
	if base == "" {
		base = `C:\ProgramData`
	}
	root, err := os.MkdirTemp(base, nativeSCMCleanupRootPrefix)
	if err != nil {
		t.Fatalf("create task-owned SCM fixture directory: %v", err)
	}
	cleanupReady := true
	t.Cleanup(func() {
		if !cleanupReady {
			t.Errorf("preserving task fixture directory because its SCM service was not confirmed deleted: %q", root)
			return
		}
		if !filepath.IsAbs(root) || filepath.Clean(root) != root || !strings.HasPrefix(strings.ToLower(filepath.Base(root)), strings.ToLower(nativeSCMCleanupRootPrefix)) {
			t.Errorf("refusing cleanup outside the task-owned fixture directory: %q", root)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove task-owned SCM fixture directory: %v", err)
		}
	})
	return root, &cleanupReady
}

func nativeSCMCleanupExecutable(t *testing.T, root string) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(source)
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "updated.test.exe")
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err != nil {
		_ = input.Close()
		t.Fatal(err)
	}
	_, copyErr := io.Copy(output, input)
	syncErr := output.Sync()
	outputCloseErr := output.Close()
	inputCloseErr := input.Close()
	if err := errors.Join(copyErr, syncErr, outputCloseErr, inputCloseErr); err != nil {
		t.Fatalf("copy task test executable: %v", err)
	}
	return destination
}

func nativeSCMCleanupOwnerSID(t *testing.T) string {
	t.Helper()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("generate task owner identity: %v", err)
	}
	now := uint64(time.Now().UnixNano())
	layoutHash := sha256.Sum256(append([]byte(filepath.Clean(os.TempDir())), random[:]...))
	ownerSID := fmt.Sprintf("S-1-5-21-%d-%d-%d-%d", uint32(now>>32), uint32(now), uint32(os.Getpid()), binary.LittleEndian.Uint32(layoutHash[:4]))
	if _, err := windows.StringToSid(ownerSID); err != nil {
		t.Fatalf("synthesized owner SID is invalid: %v", err)
	}
	return ownerSID
}

func nativeSCMCleanupPipePath(t *testing.T) string {
	t.Helper()
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatalf("generate task control endpoint: %v", err)
	}
	return `\\.\pipe\PaperboatTask223-` + hex.EncodeToString(random[:])
}

func createNativeSCMCleanupService(t *testing.T, name, executable string, cleanupReady *bool, arguments ...string) {
	t.Helper()
	manager, err := mgr.Connect()
	if err != nil {
		t.Fatalf("connect to SCM: %v", err)
	}
	existing, err := manager.OpenService(name)
	if err == nil {
		closeErr := existing.Close()
		disconnectErr := manager.Disconnect()
		t.Fatalf("refusing pre-existing service collision %q: %v", name, errors.Join(closeErr, disconnectErr))
	}
	if !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		disconnectErr := manager.Disconnect()
		t.Fatalf("could not safely check service collision %q: %v", name, errors.Join(err, disconnectErr))
	}
	service, err := manager.CreateService(name, executable, mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        mgr.StartManual,
		ErrorControl:     mgr.ErrorNormal,
		DisplayName:      name,
		Description:      nativeSCMCleanupDescription,
		ServiceStartName: "LocalSystem",
	}, arguments...)
	if err != nil {
		disconnectErr := manager.Disconnect()
		t.Fatalf("create task-only SCM service %q: %v", name, errors.Join(err, disconnectErr))
	}
	*cleanupReady = false
	t.Cleanup(func() {
		if err := cleanupNativeSCMCleanupService(name); err != nil {
			t.Errorf("remove owned SCM fixture %q: %v", name, err)
			return
		}
		*cleanupReady = true
	})
	if err := errors.Join(service.Close(), manager.Disconnect()); err != nil {
		t.Fatalf("close SCM creation handles for %q: %v", name, err)
	}
}

func cleanupNativeSCMCleanupService(name string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager, err := mgr.Connect()
	if err != nil {
		return err
	}
	service, err := manager.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return manager.Disconnect()
	}
	if err != nil {
		return errors.Join(err, manager.Disconnect())
	}
	config, configErr := service.Config()
	if configErr != nil {
		return errors.Join(configErr, service.Close(), manager.Disconnect())
	}
	if config.Description != nativeSCMCleanupDescription {
		return errors.Join(errors.New("SCM fixture ownership marker changed"), service.Close(), manager.Disconnect())
	}
	status, queryErr := service.Query()
	var stopErr error
	if queryErr == nil && status.State != svc.Stopped {
		_, stopErr = service.Control(svc.Stop)
		if errors.Is(stopErr, windows.ERROR_SERVICE_NOT_ACTIVE) || errors.Is(stopErr, windows.ERROR_SERVICE_CANNOT_ACCEPT_CTRL) {
			stopErr = nil
		}
		if stopErr == nil {
			stopErr = waitNativeSCMCleanupStopped(ctx, service)
		}
	}
	if queryErr != nil || stopErr != nil {
		return errors.Join(queryErr, stopErr, service.Close(), manager.Disconnect())
	}
	deleteErr := service.Delete()
	if errors.Is(deleteErr, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		deleteErr = nil
	}
	closeErr := service.Close()
	disconnectErr := manager.Disconnect()
	if err := errors.Join(deleteErr, closeErr, disconnectErr); err != nil {
		return err
	}
	return waitNativeSCMCleanupDeleted(ctx, name)
}

func waitNativeSCMCleanupStopped(ctx context.Context, service *mgr.Service) error {
	for {
		status, err := service.Query()
		if err != nil {
			return err
		}
		if status.State == svc.Stopped {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func waitNativeSCMCleanupDeleted(ctx context.Context, name string) error {
	for {
		manager, err := mgr.Connect()
		if err != nil {
			return err
		}
		service, openErr := manager.OpenService(name)
		if errors.Is(openErr, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return manager.Disconnect()
		}
		var closeErr error
		if openErr == nil {
			closeErr = service.Close()
		}
		disconnectErr := manager.Disconnect()
		if openErr != nil && !errors.Is(openErr, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
			return errors.Join(openErr, disconnectErr)
		}
		if err := errors.Join(closeErr, disconnectErr); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("SCM fixture deletion did not complete: %w", ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func queryNativeSCMCleanupService(t *testing.T, name string) svc.Status {
	t.Helper()
	manager, err := mgr.Connect()
	if err != nil {
		t.Fatalf("connect to SCM: %v", err)
	}
	service, err := manager.OpenService(name)
	if err != nil {
		t.Fatalf("open task-only updater service: %v", errors.Join(err, manager.Disconnect()))
	}
	status, queryErr := service.Query()
	if err := errors.Join(queryErr, service.Close(), manager.Disconnect()); err != nil {
		t.Fatalf("query task-only updater service: %v", err)
	}
	return status
}

func assertNativeSCMCleanupPipeClosed(t *testing.T, pipePath string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := winio.DialPipeContext(ctx, pipePath)
	if err == nil {
		_ = connection.Close()
		t.Fatal("activator control endpoint still accepted connections")
	}
	if !errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		t.Fatalf("control endpoint close did not produce a missing-pipe result: %v", err)
	}
	listener, err := winio.ListenPipe(pipePath, &winio.PipeConfig{SecurityDescriptor: "D:P(A;;GA;;;SY)"})
	if err != nil {
		t.Fatalf("rebind task-owned control endpoint after close: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatalf("close control endpoint rebind probe: %v", err)
	}
}
