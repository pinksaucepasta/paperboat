//go:build windows

package machineguard

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/machineguard/wfp"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

const DefaultSocket = `\\.\pipe\PaperboatMachineGuard`
const DefaultStateDir = `C:\ProgramData\Paperboat\MachineGuard`

func requirePrivilege() error {
	token, err := windowssecurity.OpenEffectiveToken()
	if err != nil {
		return err
	}
	defer token.Close()
	system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil || !user.User.Sid.Equals(system) {
		return errors.New("machine guard must run as LocalSystem")
	}
	return nil
}
func protectedDirectory(path string, mode os.FileMode) error {
	_ = mode
	system, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err := validateDirectoryAncestors(path, system); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("machine guard directory is not a protected directory")
		}
		if !protectedMachineDirectory(path, system) {
			return errors.New("existing machine guard directory is not owned and protected by LocalSystem")
		}
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	missing := []string{}
	parent := filepath.Clean(path)
	for {
		info, err := os.Lstat(parent)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return errors.New("machine guard directory ancestor is not a directory")
			}
			if strings.EqualFold(filepath.Base(parent), "Paperboat") && !protectedMachineAncestor(parent, system) {
				return fmt.Errorf("existing Paperboat directory %s is not owned and protected by LocalSystem", parent)
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, parent)
		next := filepath.Dir(parent)
		if next == parent {
			return errors.New("machine guard directory has no existing ancestor")
		}
		parent = next
	}
	descriptor, err := windows.SecurityDescriptorFromString("O:SYD:P(A;OICI;GA;;;SY)(A;OICI;GA;;;BA)")
	if err != nil {
		return err
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: descriptor}
	// Only a token with SeRestorePrivilege enabled may name LocalSystem as the owner of a new object. An
	// administrator's token holds the privilege disabled (an SSH session's has it enabled, which hid this).
	return windowssecurity.WithRestorePrivilege(func() error {
		for i := len(missing) - 1; i >= 0; i-- {
			value, pointerErr := windows.UTF16PtrFromString(missing[i])
			if pointerErr != nil {
				return pointerErr
			}
			if err := windows.CreateDirectory(value, &attributes); err != nil {
				return err
			}
		}
		return nil
	})
}

func validateDirectoryAncestors(path string, system *windows.SID) error {
	clean := filepath.Clean(path)
	programData := filepath.Clean(os.Getenv("ProgramData"))
	programFiles := filepath.Clean(os.Getenv("ProgramFiles"))
	managedData := filepath.Join(programData, "Paperboat")
	managedFiles := filepath.Join(programFiles, "Paperboat")
	lower := strings.ToLower(clean)
	if lower != strings.ToLower(managedData) && !strings.HasPrefix(lower, strings.ToLower(managedData+`\`)) && lower != strings.ToLower(managedFiles) && !strings.HasPrefix(lower, strings.ToLower(managedFiles+`\`)) {
		return errors.New("machine guard directory must be under the protected Paperboat program or state directory")
	}
	for current := clean; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err == nil {
			attributes, ok := info.Sys().(*syscall.Win32FileAttributeData)
			if !ok || attributes.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || !info.IsDir() {
				return errors.New("machine guard directory path contains a reparse point or non-directory")
			}
			underManagedBase := strings.HasPrefix(strings.ToLower(current), strings.ToLower(programData+`\Paperboat`)) || strings.HasPrefix(strings.ToLower(current), strings.ToLower(programFiles+`\Paperboat`))
			if underManagedBase && !protectedMachineAncestor(current, system) {
				return errors.New("machine guard directory ancestor is not owned and protected by LocalSystem")
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		next := filepath.Dir(current)
		if next == current {
			break
		}
	}
	return nil
}

func protectedMachineAncestor(path string, system *windows.SID) bool {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		return false
	}
	owner, _, err := descriptor.Owner()
	control, _, controlErr := descriptor.Control()
	admins, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil || controlErr != nil || owner == nil || (!owner.Equals(system) && !owner.Equals(admins)) || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil {
		return false
	}
	const readExecuteMask windows.ACCESS_MASK = 0x001200a9
	for index := uint32(0); index < uint32(dacl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, index, &ace) != nil || ace == nil || ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return false
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() {
			return false
		}
		if sid.Equals(system) || sid.Equals(admins) {
			continue
		}
		if ace.Mask & ^readExecuteMask != 0 {
			return false
		}
	}
	return true
}

// Windows does not expose a directory fsync equivalent through os.File.
func syncDirectory(string) error { return nil }

func replaceStateFile(source, target string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

const guardRetiredSuffix = ".retired-"

var deleteGuardFileAtRestart = func(path *uint16) error {
	return windows.MoveFileEx(path, nil, windows.MOVEFILE_DELAY_UNTIL_REBOOT)
}

// replaceGuardFile puts source in place of the guard executable. `schtasks /End` returns before the guard's
// process has exited, and Windows refuses to replace a running image ("Access is denied"), so the replacement
// waits out that bounded shutdown (Windows Sandbox, 2026-11-10: the rollback of a failed update replaced it at
// once and left the new guard in place). An image that still runs then is moved aside (a running image can be
// renamed), as hostinstall's removeWindowsFileWithRetry does, and removed by the next call or the restart.
func replaceGuardFile(source, target string) error {
	if stale, err := filepath.Glob(target + guardRetiredSuffix + "*"); err == nil {
		for _, retired := range stale {
			_ = os.Remove(retired) // still running: stays for the next call or the restart
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := replaceStateFile(source, target)
		if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) && !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return err
		}
		if time.Now().After(deadline) {
			retired := target + guardRetiredSuffix + strconv.FormatInt(time.Now().UnixNano(), 36)
			if moveErr := os.Rename(target, retired); moveErr != nil {
				return errors.Join(err, fmt.Errorf("move the running machine guard executable aside: %w", moveErr))
			}
			if pointer, pointerErr := windows.UTF16PtrFromString(retired); pointerErr == nil {
				_ = deleteGuardFileAtRestart(pointer) // last resort; the sweep above normally removes it first
			}
			return replaceStateFile(source, target)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
func protectedMachineDirectory(path string, system *windows.SID) bool {
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil || descriptor == nil {
		return false
	}
	owner, _, err := descriptor.Owner()
	control, _, controlErr := descriptor.Control()
	if err != nil || controlErr != nil || owner == nil || !owner.Equals(system) || control&windows.SE_DACL_PROTECTED == 0 {
		return false
	}
	sddl := descriptor.String()
	start := strings.Index(sddl, "D:")
	if start < 0 {
		return false
	}
	for _, ace := range strings.Split(sddl[start:], "(")[1:] {
		end := strings.IndexByte(ace, ')')
		if end < 0 {
			return false
		}
		entry := ace[:end]
		if !strings.HasSuffix(entry, ";;;SY") && !strings.HasSuffix(entry, ";;;BA") {
			return false
		}
	}
	return true
}

type stateLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func lockState(path string) (io.Closer, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	lock := &stateLock{file: file}
	if err = windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &lock.overlapped); err != nil {
		file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, errGuardRunning
		}
		return nil, err
	}
	return lock, nil
}
func (l *stateLock) Close() error {
	unlock := windows.UnlockFileEx(windows.Handle(l.file.Fd()), 0, 1, 0, &l.overlapped)
	return errors.Join(unlock, l.file.Close())
}
func listenProtected(ctx context.Context, address, _ string) (net.Listener, error) {
	var lc net.ListenConfig
	listener, err := lc.Listen(ctx, "tcp4", address)
	return listener, windowsProtectedListenError(err)
}

// A denied bind can mean an OS-reserved range or an exclusive listener.
// Keep that native port closed without withdrawing independent browser routes.
// Access failures from other operations remain failures of the guard itself.
func windowsProtectedListenError(err error) error {
	if errors.Is(err, windows.WSAEADDRINUSE) {
		return ErrPortInUse
	}
	var operation *os.SyscallError
	if errors.As(err, &operation) && operation.Syscall == "bind" && errors.Is(operation.Err, windows.WSAEACCES) {
		return ErrPortInUse
	}
	return err
}

func applyProtection(ctx context.Context, cfg Config, leases []*guardedLease) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	values := make([]wfp.Lease, 0, len(leases))
	for _, lease := range leases {
		values = append(values, wfp.Lease{IP: lease.ip, SID: lease.uid})
	}
	return wfp.Apply(values, cfg.LoopbackCIDR, protectedLoopbackCIDRs()...)
}
func notifyReady() error    { return nil }
func retirePlatform(Config) {}
