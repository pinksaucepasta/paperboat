//go:build windows

package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
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

// purgeAccountSweep is the account-scoped inventory step of Windows teardown.
// It runs at the end of the elevated Purge, so uninstall and reset share one
// implementation. It stops and deletes this account's SCM services before it
// terminates processes started from its payload roots, because SCM restart
// policy would otherwise relaunch them. It removes the payload roots again
// after termination, applies the machine-guard rule, and then re-scans every
// item it owns. Every step is attempted, so one failure cannot hide another.
func purgeAccountSweep(ctx context.Context, layout service.Layout) error {
	return purgeAccountSweepKeepingCLI(ctx, layout, "")
}

func purgeAccountSweepKeepingCLI(ctx context.Context, layout service.Layout, keep string) error {
	instanceRoot, err := WindowsInstanceRoot(layout.Instance)
	if err != nil {
		return err
	}
	roots := []string{layout.InstallRoot, instanceRoot}
	var result error
	result = errors.Join(result, removeAccountServices(ctx, layout.Instance))
	result = errors.Join(result, terminatePayloadProcesses(roots))
	for _, root := range roots {
		if err := removeWindowsAccountPayloadKeepingCLI(ctx, root, keep); err != nil {
			result = errors.Join(result, hostInstallFailure("remove Paperboat account payload", err))
		}
	}
	result = errors.Join(result, purgeMachineGuardWhenUnshared(ctx, layout.Instance))
	leftovers, err := accountLeftoversKeepingCLI(ctx, layout, instanceRoot, keep)
	if err != nil {
		result = errors.Join(result, hostInstallFailure("re-scan Paperboat installation items", err))
	} else if len(leftovers) > 0 {
		result = errors.Join(result, hostInstallFailure("Paperboat cleanup left items behind: "+strings.Join(leftovers, ", "), errWindowsAccountLeftovers))
	}
	return result
}

// SCM shutdown and image termination can briefly leave a delete handle open.
// Keep the bounded retry in the elevated account sweep shared by uninstall and
// reset. Authorization failures never reach a separate caller-side deletion.
func removeWindowsAccountPayload(ctx context.Context, root string) error {
	return removeWindowsAccountPayloadKeepingCLI(ctx, root, "")
}

func removeWindowsAccountPayloadKeepingCLI(ctx context.Context, root, keep string) error {
	deadline := time.Now().Add(windowsAccountProcessExitTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := RemovePathPreservingCLI(ctx, root, keep)
		if err == nil {
			return nil
		}
		if !errors.Is(err, windows.ERROR_SHARING_VIOLATION) && !errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return err
		}
		if !time.Now().Before(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(err, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
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

// terminatePayloadProcesses kills processes whose executable lies under the
// account payload roots. The current process and its parent are never killed:
// the parent may be the non-elevated pb that is waiting for this purge.
func terminatePayloadProcesses(roots []string) error {
	processIDs, err := payloadProcessIDs(roots)
	if err != nil {
		return hostInstallFailure("list Paperboat processes", err)
	}
	var result error
	for _, processID := range processIDs {
		result = errors.Join(result, terminatePayloadProcess(processID))
	}
	return result
}

func terminatePayloadProcess(processID uint32) (resultErr error) {
	handle, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, processID)
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return nil
	}
	if err != nil {
		return hostInstallFailure(fmt.Sprintf("open Paperboat process %d", processID), err)
	}
	defer func() {
		resultErr = withCleanup("close Paperboat process handle failed", resultErr, windows.CloseHandle(handle))
	}()
	if err := windows.TerminateProcess(handle, 1); err != nil {
		return hostInstallFailure(fmt.Sprintf("terminate Paperboat process %d", processID), err)
	}
	result, err := windows.WaitForSingleObject(handle, uint32(windowsAccountProcessExitTimeout/time.Millisecond))
	if err != nil {
		return hostInstallFailure(fmt.Sprintf("wait for Paperboat process %d", processID), err)
	}
	if result != windows.WAIT_OBJECT_0 {
		return hostInstallFailure(fmt.Sprintf("Paperboat process %d did not exit in time", processID), context.DeadlineExceeded)
	}
	return nil
}

// payloadProcessIDs lists running processes whose image is under a root. A
// process whose image cannot be queried is not provably Paperboat payload and
// is left alone; the re-scan reports it if it still holds payload open.
func payloadProcessIDs(roots []string) ([]uint32, error) {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snapshot)
	var entries []windows.ProcessEntry32
	entry := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snapshot, &entry); err == nil; err = windows.Process32Next(snapshot, &entry) {
		entries = append(entries, entry)
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		return nil, err
	}
	parents := make(map[uint32]uint32, len(entries))
	for _, entry := range entries {
		parents[entry.ProcessID] = entry.ParentProcessID
	}
	// Reset can run from an installed CLI through multiple elevation helpers.
	// Keep its full visible invocation ancestry, as Unix teardown does.
	protected := map[uint32]bool{}
	for _, start := range []uint32{uint32(os.Getpid()), uint32(os.Getppid())} {
		for id := start; id != 0 && !protected[id]; id = parents[id] {
			protected[id] = true
		}
	}
	var processIDs []uint32
	for _, entry := range entries {
		if entry.ProcessID == 0 || protected[entry.ProcessID] {
			continue
		}
		image, ok := processImagePath(entry.ProcessID)
		if ok && windowsImageUnderRoots(image, roots) {
			processIDs = append(processIDs, entry.ProcessID)
		}
	}
	return processIDs, nil
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

// accountLeftovers gathers the post-sweep inventory for this account only.
// Other accounts' items are not leftovers of this purge.
func accountLeftovers(layout service.Layout, instanceRoot string) ([]string, error) {
	return accountLeftoversKeepingCLI(context.Background(), layout, instanceRoot, "")
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

func accountLeftoversKeepingCLI(ctx context.Context, layout service.Layout, instanceRoot, keep string) ([]string, error) {
	names, err := listWindowsServiceNames()
	if err != nil {
		return nil, err
	}
	processIDs, err := payloadProcessIDs([]string{layout.InstallRoot, instanceRoot})
	if err != nil {
		return nil, err
	}
	var directories []string
	for _, item := range []struct{ label, path string }{
		{"Program Files install directory", layout.InstallRoot},
		{"ProgramData instance directory", instanceRoot},
	} {
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
