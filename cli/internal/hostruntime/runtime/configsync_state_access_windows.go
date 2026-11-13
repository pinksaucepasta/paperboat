//go:build windows

package runtime

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// repairConfigSyncStateAccess rebinds the sync state of every assignment under root to the owner's stable SID.
// A worker's S4U logon SID is temporary and what a worker or its git creates can name only that session, so the
// next worker could neither prune nor read it: os.RemoveAll answered "access denied" on every 30 s retry, the
// factory never finished and the apply stayed pending (victus, 2026-11-14). The owner may always rewrite the
// DACL of the objects it owns, which needs no privilege.
func repairConfigSyncStateAccess(root string) error {
	token, err := windowssecurity.OpenEffectiveToken()
	if err != nil {
		return fmt.Errorf("repair config sync state access: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user == nil || user.User.Sid == nil {
		return fmt.Errorf("repair config sync state access: no user SID: %w", err)
	}
	owner := user.User.Sid.String()
	entries, err := os.ReadDir(root)
	if err != nil {
		return fmt.Errorf("repair config sync state access: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() || !assignmentStateName.MatchString(entry.Name()) {
			continue
		}
		err := filepath.WalkDir(filepath.Join(root, entry.Name()), func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.Type()&^fs.ModeDir != 0 { // a link or other irregular object: SetNamedSecurityInfo would follow it
				return nil
			}
			access := "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + owner + ")"
			if d.IsDir() {
				access = windowssecurity.OwnerFullControlDirectoryDACL(owner)
			}
			return setStateDACL(path, access)
		})
		if err != nil {
			return fmt.Errorf("repair config sync state access: %w", err)
		}
	}
	return nil
}

func setStateDACL(path, access string) error {
	descriptor, err := windows.SecurityDescriptorFromString(access)
	if err != nil {
		return err
	}
	acl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
