//go:build windows

package machineguard

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/machineguard/wfp"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
	"golang.org/x/sys/windows"
)

const windowsGuardExecutableName = "pb.exe"

// windowsLocalRootSubject is the common name of every local browser root the
// guard installs. It is used only when the retained certificate state is gone.
const windowsLocalRootSubject = "Paperboat Local Root CA"

// Purge completely removes the machine guard, including the persistent WFP
// address protection that Uninstall deliberately retains. It is used only when
// no Paperboat installation remains. Every step runs; failures are joined.
func Purge(ctx context.Context) error {
	if !elevation.IsCurrentProcessElevated() {
		return cleanupFailure(CleanupPrecondition, errors.New("removing the machine guard requires an elevated administrator"))
	}
	programFiles := os.Getenv("ProgramFiles")
	if programFiles == "" {
		return cleanupFailure(CleanupPrecondition, errors.New("ProgramFiles is unavailable"))
	}
	installDir := filepath.Join(programFiles, "Paperboat", "MachineGuard")
	installed := filepath.Join(installDir, windowsGuardExecutableName)
	var result error
	stateExists := false
	if _, err := os.Stat(DefaultStateDir); err == nil {
		stateExists = true
		if _, err := Uninstall(ctx); err != nil {
			result = errors.Join(result, cleanupFailure(CleanupUninstall, fmt.Errorf("guard uninstall: %w", err)))
		}
	} else if !os.IsNotExist(err) {
		result = errors.Join(result, cleanupFailure(CleanupStateInspect, err))
	}
	// The task must go before its process is killed: its restart-on-failure
	// trigger would otherwise start the guard again.
	result = errors.Join(result, cleanupFailure(CleanupTaskRemove, removeGuardScheduledTask(ctx, installed)))
	result = errors.Join(result, cleanupFailure(CleanupProcessStop, terminateGuardProcesses(installed)))
	result = errors.Join(result, cleanupFailure(CleanupWFPRemove, wfp.Remove(ctx)))
	result = errors.Join(result, cleanupFailure(CleanupHostsCleanup, cleanupHistoricalLocalNames(ctx, Config{StateDir: DefaultStateDir})))
	result = errors.Join(result, cleanupFailure(CleanupCertificatesCleanup, removeGuardRoots(ctx, stateExists)))
	result = errors.Join(result, cleanupFailure(CleanupPayloadRemove, removeOwnedPath(installDir)))
	// The retained state holds the lease journal and root records that the steps
	// above read. Remove it only after they all succeeded.
	if result == nil {
		result = errors.Join(result, cleanupFailure(CleanupStateRemove, removeOwnedPath(DefaultStateDir)))
	} else {
		result = errors.Join(result, fmt.Errorf("machine guard state %s retained; fix the errors above and retry", DefaultStateDir))
	}
	// Parents are shared with user payloads and are removed only when empty.
	result = errors.Join(result, cleanupFailure(CleanupPayloadParentRemove, removeEmptyDirectory(filepath.Dir(installDir))))
	result = errors.Join(result, cleanupFailure(CleanupStateParentRemove, removeEmptyDirectory(filepath.Dir(DefaultStateDir))))
	return result
}

// removeGuardScheduledTask deletes the guard task only when it is Paperboat's
// own. A foreign task with the same name is reported and preserved.
func removeGuardScheduledTask(ctx context.Context, installed string) error {
	owned, err := ownedScheduledTask(ctx, installed)
	if err != nil {
		return err
	}
	if !owned {
		return nil
	}
	if output, err := exec.CommandContext(ctx, "schtasks.exe", "/Delete", "/TN", windowsGuardTask, "/F").CombinedOutput(); err != nil {
		return fmt.Errorf("remove machine guard task: %w: %s", err, output)
	}
	return nil
}

// terminateGuardProcesses stops every process running the exact guard
// executable. Only processes named pb.exe are opened, so protected system
// processes are never probed.
func terminateGuardProcesses(executable string) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return fmt.Errorf("list processes: %w", err)
	}
	defer windows.CloseHandle(snapshot)
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	var result error
	err = windows.Process32First(snapshot, &entry)
	for ; err == nil; err = windows.Process32Next(snapshot, &entry) {
		name := windows.UTF16ToString(entry.ExeFile[:])
		if entry.ProcessID == 0 || entry.ProcessID == uint32(os.Getpid()) || !strings.EqualFold(name, windowsGuardExecutableName) {
			continue
		}
		result = errors.Join(result, terminateIfRunningFrom(entry.ProcessID, executable))
	}
	if !errors.Is(err, windows.ERROR_NO_MORE_FILES) {
		result = errors.Join(result, err)
	}
	return result
}

func terminateIfRunningFrom(pid uint32, executable string) error {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			// The process exited between enumeration and open.
			return nil
		}
		return fmt.Errorf("open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)
	buffer := make([]uint16, windows.MAX_PATH*128)
	size := uint32(len(buffer))
	if err := windows.QueryFullProcessImageName(handle, 0, &buffer[0], &size); err != nil {
		return fmt.Errorf("inspect process %d: %w", pid, err)
	}
	if !sameExecutablePath(windows.UTF16ToString(buffer[:size]), executable) {
		return nil
	}
	if err := windows.TerminateProcess(handle, 1); err != nil {
		return fmt.Errorf("stop machine guard process %d: %w", pid, err)
	}
	if event, err := windows.WaitForSingleObject(handle, 10000); err != nil || event != windows.WAIT_OBJECT_0 {
		return fmt.Errorf("machine guard process %d did not exit: %w", pid, err)
	}
	return nil
}

func sameExecutablePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// removeGuardRoots removes the owned roots recorded in state, then sweeps the
// LocalMachine ROOT store for any Paperboat local root whose state was lost.
func removeGuardRoots(ctx context.Context, stateExists bool) error {
	var result error
	if stateExists {
		roots, err := uninstallRoots(DefaultStateDir)
		if err != nil {
			result = err
		} else {
			result = cleanupWindowsOwnedTrust(ctx, roots)
		}
	}
	return errors.Join(result, removeLocalRootsBySubject(ctx))
}

func removeLocalRootsBySubject(ctx context.Context) error {
	name, _ := windows.UTF16PtrFromString("ROOT")
	store, err := windows.CertOpenStore(windows.CERT_STORE_PROV_SYSTEM_W, 0, 0, windows.CERT_SYSTEM_STORE_LOCAL_MACHINE|windows.CERT_STORE_OPEN_EXISTING_FLAG, uintptr(unsafe.Pointer(name)))
	if err != nil {
		return err
	}
	defer windows.CertCloseStore(store, 0)
	var previous *windows.CertContext
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cert, e := windows.CertEnumCertificatesInStore(store, previous)
		if e != nil {
			if errors.Is(e, windows.Errno(windows.CRYPT_E_NOT_FOUND)) {
				return nil
			}
			return e
		}
		if cert == nil {
			return nil
		}
		previous = cert
		if isPaperboatLocalRoot(unsafe.Slice(cert.EncodedCert, cert.Length)) {
			if e = windows.CertDeleteCertificateFromStore(cert); e != nil {
				return e
			}
			previous = nil
		}
	}
}

func isPaperboatLocalRoot(der []byte) bool {
	certificate, err := x509.ParseCertificate(der)
	return err == nil && strings.Contains(certificate.Subject.CommonName, windowsLocalRootSubject)
}

// removeEmptyDirectory removes path only when nothing else remains in it.
func removeEmptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return nil
	}
	return os.Remove(path)
}

func removeOwnedPath(path string) error {
	if err := os.RemoveAll(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
