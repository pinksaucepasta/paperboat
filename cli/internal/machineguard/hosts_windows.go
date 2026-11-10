//go:build windows

package machineguard

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	// The staged file already inherits the parent's rules, as the original does. Give it the original's owner and group,
	// and its DACL only when it has explicit rules or blocks inheritance: passing the full DACL back (inherited
	// rules included) would add every inherited rule a second time as an explicit one.
	flags := windows.SECURITY_INFORMATION(windows.OWNER_SECURITY_INFORMATION | windows.GROUP_SECURITY_INFORMATION)
	var explicitDACL *windows.ACL
	protected := control&windows.SE_DACL_PROTECTED != 0
	explicit, err := explicitACEs(dacl)
	if err != nil {
		return err
	}
	if protected && len(explicit) == 0 {
		return errors.New("hosts file blocks inheritance without access rules; left unchanged")
	}
	if protected || len(explicit) > 0 {
		if explicitDACL, err = windows.ACLFromEntries(explicit, nil); err != nil {
			return err
		}
		flags |= windows.DACL_SECURITY_INFORMATION | windows.UNPROTECTED_DACL_SECURITY_INFORMATION
		if protected {
			flags = flags&^windows.UNPROTECTED_DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
		}
	}
	if err = windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(staged, windows.SE_FILE_OBJECT, flags, owner, group, explicitDACL, nil)
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
	var failed []string
	if !bytes.Equal(displaced, original) {
		failed = append(failed, "the displaced file differs from the original")
	}
	if !bytes.Equal(installed, next) {
		failed = append(failed, "the installed file differs from the update")
	}
	if !sameFileSecurity(installedSD, security) {
		failed = append(failed, "the installed file's security differs from the original's")
	}
	if len(failed) > 0 {
		return fmt.Errorf("hosts changed during replacement or security verification failed (%s); displaced file retained at %s", strings.Join(failed, "; "), backup)
	}
	return os.Remove(backup)
}
