//go:build windows

package localdaemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

func TestNativeWindowsOwnerIdentityFacts(t *testing.T) {
	ownerSID := strings.TrimSpace(os.Getenv("PAPERBOAT_DIAG_OWNER_SID"))
	lockPath := strings.TrimSpace(os.Getenv("PAPERBOAT_DIAG_LOCK"))
	if ownerSID == "" && lockPath == "" {
		t.Skip("requires the protected native owner identity inputs")
	}
	if ownerSID == "" || lockPath == "" {
		t.Fatal("native owner identity inputs are incomplete")
	}
	token, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil || token == nil || token.User.Sid == nil {
		t.Fatal("cannot identify native test process token")
	}
	if token.User.Sid.String() != "S-1-5-18" {
		t.Skip("requires SYSTEM token")
	}

	ownerSIDValid := validateWindowsOwnerSID(ownerSID) == nil
	lockPathValid := filepath.IsAbs(lockPath) && filepath.Clean(lockPath) == lockPath
	layout, layoutErr := service.WindowsUserLayout(ownerSID)
	ownerLayoutReady := ownerSIDValid && layoutErr == nil && layout.Instance != ""

	var record windowsDaemonPIDLock
	lockRead := false
	if ownerSIDValid && lockPathValid {
		var readErr error
		record, readErr = defaultReadWindowsDaemonPIDLock(lockPath, ownerSID)
		lockRead = readErr == nil
	}

	var identity windowsDaemonProcessIdentity
	processOpened, processIdentityRead, processClosed := false, false, false
	if lockRead && record.Record.PID != 0 {
		process, openErr := defaultOpenWindowsDaemonProcess(record.Record.PID)
		processOpened = openErr == nil && process != nil
		if processOpened {
			var identityErr error
			identity, identityErr = process.Identity()
			processIdentityRead = identityErr == nil
			processClosed = process.Close() == nil
		}
	}

	pidMatches := processIdentityRead && identity.PID == record.Record.PID
	creationTimeMatches := processIdentityRead && !identity.CreationTime.IsZero() && identity.CreationTime.UnixNano() == record.Record.CreationTimeUnixNano
	ownerSIDMatches := processIdentityRead && strings.EqualFold(identity.OwnerSID, ownerSID)
	recordedImageStable, recordedImageRollback := false, false
	processImageStable, processImageRollback, processImageInVersionSlot := false, false, false
	managedCommandShape, daemonServerCommandShape := false, false
	argumentHashMatches := false
	validatorAccepts := false
	if processIdentityRead && ownerLayoutReady {
		recordedImageStable = strings.EqualFold(filepath.Clean(record.Record.Executable), filepath.Clean(layout.Binary))
		recordedImageRollback = strings.EqualFold(filepath.Clean(record.Record.Executable), filepath.Clean(layout.BinaryRollback))
		processImageStable = strings.EqualFold(filepath.Clean(identity.Executable), filepath.Clean(layout.Binary))
		processImageRollback = strings.EqualFold(filepath.Clean(identity.Executable), filepath.Clean(layout.BinaryRollback))
		processImageInVersionSlot = nativeWindowsOwnerImageInLayout(identity.Executable, layout)
		managedCommandShape = len(identity.Arguments) == 5 && identity.Arguments[1] == "daemon" && identity.Arguments[2] == "__runtime-local-daemon" && identity.Arguments[3] == "--instance" && identity.Arguments[4] == layout.Instance
		daemonServerCommandShape = len(identity.Arguments) == 4 && identity.Arguments[1] == "daemon" && identity.Arguments[2] == "--server" && validTaskText(identity.Arguments[3])

		hashExecutable := identity.Executable
		if !strings.EqualFold(filepath.Clean(identity.Executable), filepath.Clean(record.Record.Executable)) && recordedImageStable && processImageRollback {
			hashExecutable = record.Record.Executable
		}
		normalizedExecutable, digest, hashErr := normalizedWindowsDaemonIdentity(hashExecutable, identity.Arguments)
		argumentHashMatches = hashErr == nil && strings.EqualFold(normalizedExecutable, record.Record.Executable) && strings.EqualFold(digest, record.Record.ArgumentsSHA256)
		validatorAccepts = validateOwnedWindowsDaemon(identity, record, ownerSID) == nil
	}

	_, scmPID, scmRunning := nativeWindowsOwnerServicePID(ownerSID)
	scmPIDMatchesLock := scmRunning && scmPID == record.Record.PID
	t.Logf("native owner identity facts: owner_sid_valid=%t lock_path_valid=%t owner_layout_ready=%t lock_read=%t process_opened=%t process_identity_read=%t process_closed=%t pid_matches=%t creation_time_matches=%t owner_sid_matches=%t arguments_hash_matches=%t recorded_image_stable=%t recorded_image_rollback=%t process_image_stable=%t process_image_rollback=%t process_image_in_version_slot=%t managed_command_shape_exact_instance=%t daemon_server_command_shape=%t scm_running=%t scm_pid_matches_lock=%t validator_accepts=%t",
		ownerSIDValid, lockPathValid, ownerLayoutReady, lockRead, processOpened, processIdentityRead, processClosed, pidMatches, creationTimeMatches, ownerSIDMatches, argumentHashMatches, recordedImageStable, recordedImageRollback, processImageStable, processImageRollback, processImageInVersionSlot, managedCommandShape, daemonServerCommandShape, scmRunning, scmPIDMatchesLock, validatorAccepts)
	if !ownerSIDValid || !lockPathValid || !ownerLayoutReady || !lockRead || !processOpened || !processIdentityRead || !processClosed || !validatorAccepts {
		t.Fatal("native owner process identity did not satisfy the current ownership validator")
	}
}

func nativeWindowsOwnerImageInLayout(executable string, layout service.Layout) bool {
	if !filepath.IsAbs(executable) {
		return false
	}
	clean := filepath.Clean(executable)
	if strings.EqualFold(clean, filepath.Clean(layout.Binary)) || strings.EqualFold(clean, filepath.Clean(layout.BinaryRollback)) {
		return true
	}
	versionsRoot := filepath.Join(layout.ReleasesRoot, "versions")
	relative, err := filepath.Rel(versionsRoot, clean)
	if err != nil {
		return false
	}
	parts := strings.Split(relative, string(filepath.Separator))
	return len(parts) == 2 && parts[0] != "" && parts[0] != "." && parts[0] != ".." && filepath.Base(parts[0]) == parts[0] && strings.EqualFold(parts[1], "pb.exe")
}

func nativeWindowsOwnerServicePID(ownerSID string) (name string, pid uint32, running bool) {
	name, err := windowsOwnerServiceName(ownerSID)
	if err != nil {
		return "", 0, false
	}
	manager, err := mgr.Connect()
	if err != nil {
		return name, 0, false
	}
	defer manager.Disconnect()
	item, err := manager.OpenService(name)
	if err != nil {
		return name, 0, false
	}
	defer item.Close()
	status, err := item.Query()
	if err != nil {
		return name, 0, false
	}
	return name, status.ProcessId, status.State == svc.Running && status.ProcessId != 0
}
