//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	windowsAccountServiceStopTimeout = 30 * time.Second
	windowsAccountProcessExitTimeout = 5 * time.Second
	// Mirrors the machine PATH key used by internal/windowsenvironment.
	windowsMachineEnvironmentKey = `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`
)

var errWindowsAccountLeftovers = errors.New("Paperboat installation items remain after the account sweep")

// ErrRemovalAtRestart is a purge that removed everything except files another program still holds open. Those are
// moved aside and registered with Windows for deletion at the next restart; nothing else of the account remains.
var ErrRemovalAtRestart = errors.New("Paperboat was removed; files that are still in use are deleted when Windows restarts")

// purgeAccountSweepKeepingCLI is the account-scoped inventory step of Windows teardown.
// It runs at the end of the elevated Purge, so uninstall and reset share one
// implementation. It stops and deletes this account's SCM services before it
// terminates processes started from its payload roots, because SCM restart
// policy would otherwise relaunch them. Only then does it remove the payload
// roots (an old CLI still running from the rollback slot holds its image until
// it is ended), applies the machine-guard rule, and re-scans every item it owns.
// Every step is attempted, so one failure cannot hide another. atRestart reports
// that files in use were left to Windows to delete at the next restart.
func purgeAccountSweepKeepingCLI(ctx context.Context, layout service.Layout, ownerSID, keep string) (atRestart bool, _ error) {
	instanceRoot, err := WindowsInstanceRoot(layout.Instance)
	if err != nil {
		return false, err
	}
	roots := []string{layout.InstallRoot, instanceRoot}
	var result error
	result = errors.Join(result, removeAccountServices(ctx, layout.Instance))
	result = errors.Join(result, terminatePayloadProcesses(roots, ownerSID))
	var deferred []string
	for _, root := range []string{layout.InstallRoot, layout.UpdateStateRoot, instanceRoot} {
		pending, err := removeWindowsAccountPayloadKeepingCLI(ctx, root, keep)
		if err != nil {
			result = errors.Join(result, hostInstallFailure("remove Paperboat account payload", err))
		} else if pending {
			deferred = append(deferred, root)
		}
	}
	result = errors.Join(result, purgeMachineGuardWhenUnshared(ctx, layout.Instance))
	leftovers, err := accountLeftoversKeepingCLI(ctx, layout, ownerSID, instanceRoot, keep, deferred)
	if err != nil {
		result = errors.Join(result, hostInstallFailure("re-scan Paperboat installation items", err))
	} else if len(leftovers) > 0 {
		result = errors.Join(result, hostInstallFailure("Paperboat cleanup left items behind: "+strings.Join(leftovers, ", "), errWindowsAccountLeftovers))
	}
	return len(deferred) > 0, result
}

// removeWindowsAccountPayloadKeepingCLI removes one owned root. SCM shutdown and image termination can briefly
// leave a handle or the image mapping open, so a busy file is retried for a bounded time. What is still in use
// after that cannot be deleted by anyone before its holder exits: it is left to Windows (atRestart).
func removeWindowsAccountPayloadKeepingCLI(ctx context.Context, root, keep string) (atRestart bool, _ error) {
	deadline := time.Now().Add(windowsAccountProcessExitTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		err := RemovePathPreservingCLI(ctx, root, keep)
		if err == nil {
			return false, nil
		}
		if !windowsFileBusy(err) {
			return false, err
		}
		if !time.Now().Before(deadline) {
			return deleteWindowsPayloadAtRestart(root, keep)
		}
		select {
		case <-ctx.Done():
			return false, errors.Join(err, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// A running program's image answers ERROR_ACCESS_DENIED to deletion, an open handle a sharing or lock violation,
// and a directory whose deleted child is still held open is not empty yet.
func windowsFileBusy(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_DIR_NOT_EMPTY)
}

// deleteWindowsPayloadAtRestart registers what is still in use under root for deletion at restart (MoveFileEx
// MOVEFILE_DELAY_UNTIL_REBOOT, the documented way to remove a file in use). A file is first moved aside under a
// retired name, as the update does: a reinstall before the restart reuses the original names, and Windows must not
// delete those. Directories are registered after their content; Windows removes them only if they are empty then.
func deleteWindowsPayloadAtRestart(root, keep string) (atRestart bool, _ error) {
	var directories []string
	var result error
	register := func(path string) {
		pointer, err := windows.UTF16PtrFromString(path)
		if err == nil {
			err = deleteWindowsFileAtRestart(pointer)
		}
		if err != nil {
			result = errors.Join(result, hostInstallFailure("register a file in use for deletion at restart", err))
			return
		}
		atRestart = true
	}
	// WalkDir does not descend into reparse points; they are removed as entries.
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if installNotExistOnly(err) {
				return nil
			}
			return err
		}
		if keep != "" && cliWithin(path, keep) {
			return nil // the retained CLI and the directories that hold it
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		removeErr := os.Remove(path)
		if removeErr == nil || installNotExistOnly(removeErr) {
			return nil
		}
		if !windowsFileBusy(removeErr) {
			result = errors.Join(result, removeErr)
			return nil
		}
		if !strings.Contains(entry.Name(), windowsRetiredSuffix) {
			retired := path + windowsRetiredSuffix + strconv.FormatInt(time.Now().UnixNano(), 36)
			//paperboat:allow-source-policy atomic-replacement owner=windows-host-install reason=retire-file-in-use-that-cannot-be-deleted
			if err := os.Rename(path, retired); err != nil {
				result = errors.Join(result, hostInstallFailure("a file in use could not be moved aside", removeErr, err))
				return nil
			}
			path = retired
		}
		register(path)
		return nil
	})
	result = errors.Join(result, walkErr)
	for index := len(directories) - 1; index >= 0; index-- {
		if err := os.Remove(directories[index]); err != nil && !installNotExistOnly(err) {
			if atRestart {
				register(directories[index])
			} else {
				result = errors.Join(result, err)
			}
		}
	}
	if result != nil {
		return false, result
	}
	return atRestart, nil
}

func removeAccountServices(ctx context.Context, instance string) (resultErr error) {
	names, err := listWindowsServiceNames()
	if err != nil {
		return hostInstallFailure("list Windows services", err)
	}
	owned := windowsAccountServices(names, instance)
	if len(owned) == 0 {
		return nil
	}
	manager, err := mgr.Connect()
	if err != nil {
		return hostInstallFailure("connect to the Windows service manager", err)
	}
	defer func() {
		resultErr = withCleanup("Windows service manager disconnect failed", resultErr, manager.Disconnect())
	}()
	var result error
	for _, name := range owned {
		result = errors.Join(result, stopAndDeleteAccountService(ctx, manager, name))
	}
	return result
}

// listWindowsServiceNames reads the SCM inventory with the same bounded buffer
// as the other SCM sweeps. Truncated inventories fail rather than hiding names.
func listWindowsServiceNames() (names []string, resultErr error) {
	manager, err := mgr.Connect()
	if err != nil {
		return nil, err
	}
	defer func() {
		resultErr = withCleanup("Windows service manager disconnect failed", resultErr, manager.Disconnect())
	}()
	buffer := make([]byte, windowsPinSCMBytes)
	var needed, count uint32
	if err := windows.EnumServicesStatusEx(manager.Handle, windows.SC_ENUM_PROCESS_INFO, windows.SERVICE_WIN32, windows.SERVICE_STATE_ALL, &buffer[0], uint32(len(buffer)), &needed, &count, nil, nil); err != nil {
		return nil, err
	}
	if uint64(count)*uint64(unsafe.Sizeof(windows.ENUM_SERVICE_STATUS_PROCESS{})) > uint64(len(buffer)) {
		return nil, ErrInvalidRequest
	}
	entries := unsafe.Slice((*windows.ENUM_SERVICE_STATUS_PROCESS)(unsafe.Pointer(&buffer[0])), int(count))
	for _, entry := range entries {
		names = append(names, windows.UTF16PtrToString(entry.ServiceName))
	}
	runtime.KeepAlive(buffer)
	return names, nil
}

// stopAndDeleteAccountService stops a declaration with a bounded wait and then
// deletes it. A declaration that is already gone or already marked for delete
// is success, so repeated purges converge.
func stopAndDeleteAccountService(ctx context.Context, manager *mgr.Mgr, name string) (resultErr error) {
	current, err := manager.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return nil
	}
	if err != nil {
		return hostInstallFailure("open Windows service "+name, err)
	}
	defer func() {
		resultErr = withCleanup("close Windows service handle failed", resultErr, current.Close())
	}()
	status, err := current.Query()
	if err != nil {
		return hostInstallFailure("query Windows service "+name, err)
	}
	if status.State != svc.Stopped {
		if status.State != svc.StopPending {
			if _, err := current.Control(svc.Stop); err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
				return hostInstallFailure("stop Windows service "+name, err)
			}
		}
		deadline := time.Now().Add(windowsAccountServiceStopTimeout)
		for status.State != svc.Stopped {
			if !time.Now().Before(deadline) {
				return hostInstallFailure("Windows service "+name+" did not stop in time", context.DeadlineExceeded)
			}
			select {
			case <-ctx.Done():
				return hostInstallFailure("stop Windows service "+name, ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
			if status, err = current.Query(); err != nil {
				return hostInstallFailure("query Windows service "+name, err)
			}
		}
	}
	if err := current.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return hostInstallFailure("delete Windows service "+name, err)
	}
	return nil
}

// terminatePayloadProcesses ends the account's processes that run an image under the account payload roots
// (windowsPayloadProcessIDs). All of them are ended first and then awaited against one deadline, so the step is
// bounded however many there are.
func terminatePayloadProcesses(roots []string, ownerSID string) (result error) {
	processIDs, err := payloadProcessIDs(roots, ownerSID)
	if err != nil {
		return hostInstallFailure("list Paperboat processes", err)
	}
	ended := map[uint32]windows.Handle{}
	defer func() {
		for _, handle := range ended {
			result = withCleanup("close Paperboat process handle failed", result, windows.CloseHandle(handle))
		}
	}()
	for _, processID := range processIDs {
		handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, processID)
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			continue // already gone
		}
		if err != nil {
			result = errors.Join(result, hostInstallFailure(fmt.Sprintf("open Paperboat process %d", processID), err))
			continue
		}
		if err := windows.TerminateProcess(handle, 1); err != nil {
			result = errors.Join(result, hostInstallFailure(fmt.Sprintf("terminate Paperboat process %d", processID), err), windows.CloseHandle(handle))
			continue
		}
		ended[processID] = handle
	}
	deadline := time.Now().Add(windowsAccountProcessExitTimeout)
	for processID, handle := range ended {
		wait := max(time.Until(deadline), 0)
		state, err := windows.WaitForSingleObject(handle, uint32(wait/time.Millisecond))
		if err != nil {
			result = errors.Join(result, hostInstallFailure(fmt.Sprintf("wait for Paperboat process %d", processID), err))
		} else if state != windows.WAIT_OBJECT_0 {
			result = errors.Join(result, hostInstallFailure(fmt.Sprintf("Paperboat process %d did not exit in time", processID), context.DeadlineExceeded))
		}
	}
	return result
}

// payloadProcessIDs lists the account's running processes (windowsPayloadProcessIDs). The current process and
// its ancestry are never listed: the parent may be the non-elevated pb that is waiting for this purge.
func payloadProcessIDs(roots []string, ownerSID string) ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	var entries []windowsProcessEntry
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		entries = append(entries, windowsProcessEntry{id: entry.ProcessID, parent: entry.ParentProcessID})
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	users := []string{ownerSID, "S-1-5-18"}
	if self, ok := processUserSID(uint32(os.Getpid())); ok {
		users = append(users, self)
	}
	keep := []uint32{uint32(os.Getpid()), uint32(os.Getppid())}
	return windowsPayloadProcessIDs(entries, keep, roots, users, processImagePath, processUserSID), nil
}

func processUserSID(processID uint32) (string, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, processID)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(handle)
	var token windows.Token
	if err := windows.OpenProcessToken(handle, windows.TOKEN_QUERY, &token); err != nil {
		return "", false
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user.User.Sid == nil {
		return "", false
	}
	return user.User.Sid.String(), true
}

func processImagePath(processID uint32) (string, bool) {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, processID)
	if err != nil {
		return "", false
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, 32768)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return "", false
	}
	return windows.UTF16ToString(buffer[:size]), true
}

// purgeMachineGuardWhenUnshared removes the machine-wide guard only when no
// other account directory remains under either Paperboat users root.
func purgeMachineGuardWhenUnshared(ctx context.Context, instance string) error {
	entries, err := windowsAccountDirectoryEntries(ctx)
	if err != nil {
		return hostInstallFailure("inspect Paperboat account directories", err)
	}
	if len(windowsOtherAccountEntries(entries, instance)) > 0 {
		return nil
	}
	if err := machineguard.Purge(ctx); err != nil {
		return hostInstallFailure("remove Paperboat machine guard", err)
	}
	return nil
}

func windowsAccountDirectoryEntries(ctx context.Context) ([]string, error) {
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		return nil, err
	}
	var entries []string
	for _, directory := range []string{filepath.Join(layout.InstallRoot, "users"), filepath.Join(WindowsProgramDataRoot(), "users")} {
		items, err := os.ReadDir(directory)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if directory == filepath.Join(layout.InstallRoot, "users") && validWindowsInstance(item.Name()) {
				accountLayout, err := WindowsLayoutForInstance(item.Name())
				if err != nil {
					return nil, err
				}
				onlyCLI, err := CLIOnlyRemains(ctx, accountLayout.InstallRoot, accountLayout.Binary)
				if err != nil {
					return nil, err
				}
				if onlyCLI {
					// Reset survivors do not own runtime or machine guard state.
					continue
				}
			}
			entries = append(entries, item.Name())
		}
	}
	return entries, nil
}

// Discover declarations independently of SCM: stopped or interrupted installs
// can leave a valid owned descriptor after the registration has disappeared.
func windowsAccountDeclarationInventory(ctx context.Context, instance string) ([]string, error) {
	root := `C:\ProgramData\Paperboat\services`
	if _, err := os.Lstat(root); installNotExistOnly(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	if err := rejectWindowsReparseAncestors(root); err != nil {
		return nil, err
	}
	file, err := os.Open(root)
	if err != nil {
		return nil, err
	}
	maximum := 2*runtimeconfig.DefaultResources.MaxSessions + 32
	entries, readErr := file.ReadDir(maximum + 1)
	closeErr := file.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return nil, errors.Join(readErr, closeErr)
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(entries) > maximum {
		return nil, ErrInvalidRequest
	}
	var paths []string
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			continue
		}
		name := strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
		if strings.EqualFold(name, "PaperboatUpdateActivator") || !windowsAccountServiceOwned(name, instance) {
			continue
		}
		if entry.IsDir() {
			return nil, ErrInvalidRequest
		}
		paths = append(paths, filepath.Join(root, entry.Name()))
	}
	return paths, nil
}

// accountLeftoversKeepingCLI gathers the post-sweep inventory for this account only. Other accounts' items are not
// leftovers of this purge, and neither is a root whose remaining files Windows deletes at restart (deferred).
func accountLeftoversKeepingCLI(ctx context.Context, layout service.Layout, ownerSID, instanceRoot, keep string, deferred []string) ([]string, error) {
	names, err := listWindowsServiceNames()
	if err != nil {
		return nil, err
	}
	processIDs, err := payloadProcessIDs([]string{layout.InstallRoot, instanceRoot}, ownerSID)
	if err != nil {
		return nil, err
	}
	var directories []string
	for _, item := range []struct{ label, path string }{
		{"Program Files install directory", layout.InstallRoot},
		{"ProgramData update state directory", layout.UpdateStateRoot},
		{"ProgramData instance directory", instanceRoot},
	} {
		if slices.Contains(deferred, item.path) {
			continue
		}
		if _, err := os.Stat(item.path); err == nil {
			onlyCLI, err := CLIOnlyRemains(ctx, item.path, keep)
			if err != nil {
				return nil, err
			}
			if !onlyCLI {
				directories = append(directories, item.label)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	pathEntry, err := machinePathHasEntry(filepath.Dir(layout.Binary))
	if err != nil {
		return nil, err
	}
	leftovers := windowsAccountLeftovers(windowsAccountServices(names, layout.Instance), processIDs, directories, pathEntry && keep == "")
	declarations, err := windowsAccountDeclarationInventory(ctx, layout.Instance)
	if err != nil {
		return nil, err
	}
	if len(declarations) > 0 {
		leftovers = append(leftovers, "service declarations")
	}
	return leftovers, nil
}

func machinePathHasEntry(directory string) (found bool, resultErr error) {
	key, err := registry.OpenKey(registry.LOCAL_MACHINE, windowsMachineEnvironmentKey, registry.QUERY_VALUE)
	if err != nil {
		return false, err
	}
	defer func() {
		resultErr = withCleanup("close machine environment key failed", resultErr, key.Close())
	}()
	value, _, err := key.GetStringValue("Path")
	if errors.Is(err, registry.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return windowsPathListContains(value, directory), nil
}
