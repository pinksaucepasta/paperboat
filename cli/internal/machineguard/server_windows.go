//go:build windows

package machineguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/machineguard/wfp"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const DefaultSocket = `\\.\pipe\PaperboatMachineGuard`
const DefaultStateDir = `C:\ProgramData\Paperboat\MachineGuard`

var nrptRoot = `SYSTEM\CurrentControlSet\Services\Dnscache\Parameters\DnsPolicyConfig`

const nrptOwner = "Paperboat machine guard v1"

func requirePrivilege() error {
	token, err := windows.OpenCurrentProcessToken()
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
	for i := len(missing) - 1; i >= 0; i-- {
		value, pointerErr := windows.UTF16PtrFromString(missing[i])
		if pointerErr != nil {
			return pointerErr
		}
		if err = windows.CreateDirectory(value, &attributes); err != nil {
			return err
		}
	}
	return nil
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
	var journal struct {
		IPs map[string]string `json:"ips"`
	}
	data, err := os.ReadFile(filepath.Join(cfg.StateDir, "reservations.json"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if len(data) > 0 && json.Unmarshal(data, &journal) != nil {
		return errors.New("invalid machine guard reservation journal")
	}
	known := make([]wfp.Lease, 0, len(journal.IPs))
	for ip, sid := range journal.IPs {
		known = append(known, wfp.Lease{IP: ip, SID: sid})
	}
	return wfp.Apply(values, known, cfg.LoopbackCIDR, protectedLoopbackCIDRs(cfg)...)
}
func cleanupHistoricalLocalNames(ctx context.Context, cfg Config) error {
	if err := clearMachineHosts(ctx, cfg); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, nrptRoot, registry.ENUMERATE_SUB_KEYS|registry.QUERY_VALUE)
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	if err == nil {
		names, readErr := root.ReadSubKeyNames(-1)
		root.Close()
		if readErr != nil {
			return readErr
		}
		for _, name := range names {
			if !strings.HasPrefix(name, "Paperboat-") {
				continue
			}
			path := nrptRoot + `\` + name
			key, e := registry.OpenKey(registry.LOCAL_MACHINE, path, registry.QUERY_VALUE)
			if e != nil {
				continue
			}
			owner, _, e := key.GetStringValue("PaperboatOwner")
			key.Close()
			if e != nil || owner != nrptOwner {
				return errors.New("machine guard NRPT ownership changed")
			}
			if e = registry.DeleteKey(registry.LOCAL_MACHINE, path); e != nil {
				return e
			}
		}
	}
	flushDNS()
	return ctx.Err()
}

var dnsFlush = windows.NewLazySystemDLL("dnsapi.dll").NewProc("DnsFlushResolverCache")

func flushDNS()             { _, _, _ = dnsFlush.Call() }
func notifyReady() error    { return nil }
func retirePlatform(Config) {}
