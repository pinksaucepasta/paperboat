//go:build windows

package deviceguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

var hostsReplaceFile = windows.NewLazySystemDLL("kernel32.dll").NewProc("ReplaceFileW")

func systemHostsPath() (string, error) {
	directory, err := windows.GetSystemDirectory()
	return filepath.Join(directory, "drivers", "etc", "hosts"), err
}
func flushHostsCache() { flushDNS() }

const hostsSecurity = windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION

func replaceHostsFile(ctx context.Context, path string, info os.FileInfo, original, next []byte) error {
	security, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, hostsSecurity)
	if err != nil {
		return err
	}
	owner, _, err := security.Owner()
	if err != nil {
		return err
	}
	group, _, err := security.Group()
	if err != nil {
		return err
	}
	dacl, _, err := security.DACL()
	if err != nil {
		return err
	}
	control, _, err := security.Control()
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".paperboat-hosts-")
	if err != nil {
		return err
	}
	staged := file.Name()
	defer os.Remove(staged)
	_, writeErr := file.Write(next)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err = errors.Join(writeErr, syncErr, closeErr); err != nil {
		return err
	}
	flags := windows.SECURITY_INFORMATION(hostsSecurity) | windows.UNPROTECTED_DACL_SECURITY_INFORMATION
	if control&windows.SE_DACL_PROTECTED != 0 {
		flags = windows.SECURITY_INFORMATION(hostsSecurity) | windows.PROTECTED_DACL_SECURITY_INFORMATION
	}
	if err = windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(staged, windows.SE_FILE_OBJECT, flags, owner, group, dacl, nil)
	}); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	actual, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !os.SameFile(info, current) || !bytes.Equal(actual, original) {
		return errors.New("hosts file changed during update; left unchanged; retry")
	}
	currentSD, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, hostsSecurity)
	if err != nil {
		return err
	}
	if currentSD.String() != security.String() {
		return errors.New("hosts security changed during update; left unchanged; retry")
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	backup := staged + ".previous"
	to, _ := windows.UTF16PtrFromString(path)
	from, _ := windows.UTF16PtrFromString(staged)
	saved, _ := windows.UTF16PtrFromString(backup)
	result, _, callErr := hostsReplaceFile.Call(uintptr(unsafe.Pointer(to)), uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(saved)), 0, 0, 0)
	if result == 0 {
		if _, e := os.Lstat(path); os.IsNotExist(e) {
			// Do not overwrite a file installed by another writer during recovery.
			if e = windows.MoveFileEx(saved, to, windows.MOVEFILE_WRITE_THROUGH); e != nil {
				return fmt.Errorf("hosts replacement failed (%w); original retained at %s; recovery: %v", callErr, backup, e)
			}
		}
		if data, e := os.ReadFile(path); e == nil && bytes.Equal(data, original) {
			_ = os.Remove(backup)
		}
		return fmt.Errorf("hosts replacement failed: %w; inspect retained backup %s before retry", callErr, backup)
	}
	displaced, err := os.ReadFile(backup)
	if err != nil {
		return fmt.Errorf("verify displaced hosts file (backup %s): %w", backup, err)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	installedSD, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, hostsSecurity)
	if err != nil {
		return err
	}
	if !bytes.Equal(displaced, original) || !bytes.Equal(installed, next) || installedSD.String() != security.String() {
		return fmt.Errorf("hosts changed during replacement or security verification failed; displaced file retained at %s", backup)
	}
	return os.Remove(backup)
}
