//go:build windows

package hostinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/windows"
)

func pinWindowsArtifactAncestors(path string) (func() error, error) {
	var handles []windows.Handle
	closeAll := func() error {
		var err error
		for index := len(handles) - 1; index >= 0; index-- {
			err = errors.Join(err, windows.CloseHandle(handles[index]))
		}
		return err
	}
	for current := filepath.Clean(path); ; current = filepath.Dir(current) {
		p, err := windows.UTF16PtrFromString(current)
		if err != nil {
			closeAll()
			return nil, err
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			closeAll()
			return nil, err
		}
		handles = append(handles, h)
		var info windows.ByHandleFileInformation
		if err := windows.GetFileInformationByHandle(h, &info); err != nil {
			closeAll()
			return nil, err
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
			closeAll()
			return nil, ErrInvalidRequest
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return closeAll, nil
}

func prepareWindowsAccountPinRoot(ctx context.Context, layout service.Layout, ownerSID string) (resultErr error) {
	canonical, err := service.WindowsUserLayout(ownerSID)
	if err != nil || canonical.Instance != layout.Instance || !strings.EqualFold(canonical.InstallRoot, layout.InstallRoot) {
		return ErrInvalidRequest
	}
	if _, err := os.Lstat(layout.InstallRoot); installNotExistOnly(err) {
		// A clean machine has neither the Paperboat nor users parent yet.
		// Establish those shared, read-only namespaces before the protected
		// single-directory account creation; CreateDirectory is not recursive.
		for _, parent := range []string{filepath.Dir(filepath.Dir(layout.InstallRoot)), filepath.Dir(layout.InstallRoot)} {
			if err := ensureWindowsExecutableDirectory(parent, ""); err != nil {
				return hostInstallFailure("prepare shared Windows release directory", err)
			}
		}
		return ensureWindowsMachineDirectory(layout.InstallRoot, ownerSID)
	} else if err != nil {
		return err
	}
	owner, err := windowsRuntimeTrustedOwner()
	if err != nil {
		return err
	}
	handle, _, err := openWindowsRuntimeObject(layout.InstallRoot, true)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(handle)) }()
	if checkWindowsRuntimeHandleSecurity(handle, owner, windowsRuntimeCurrentRootDACL(ownerSID)) == nil {
		return nil
	}
	// Adoption of an inherited Program Files account namespace requires its
	// protected enrollment and exact installed bytes, not a caller source claim.
	installed, err := LoadWindowsRuntimeConfigForInstance(layout.Instance)
	if err != nil {
		return err
	}
	if installed.OwnerSID != ownerSID || installed.EnrollmentPending {
		return ErrInvalidRequest
	}
	if err := installed.Source.Verify(layout.Binary); err != nil {
		return err
	}
	if installed.Source.Distribution == installsource.Official {
		if err := verifyWindowsInstalledBinary(ctx, layout.Binary, installed.Source.Architecture); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	release, err := pinWindowsArtifactAncestors(filepath.Dir(layout.InstallRoot))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	parent, err := openWindowsRuntimeSecurityObject(filepath.Dir(layout.InstallRoot))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, windows.CloseHandle(parent)) }()
	if err := checkWindowsReadonlyAccountDirectory(parent, ownerSID); err != nil {
		return err
	}
	if err := checkWindowsReadonlyAccountDirectory(handle, ownerSID); err != nil {
		return err
	}
	return applyWindowsHandleOwnedDACL(handle, owner, windowsRuntimeCurrentRootDACL(ownerSID))
}

func checkWindowsReadonlyAccountDirectory(handle windows.Handle, ownerSID string) error {
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return err
	}
	if owner.String() != "S-1-5-18" && owner.String() != "S-1-5-32-544" {
		return ErrInvalidRequest
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil {
		return errors.Join(ErrInvalidRequest, err)
	}
	const installerSID = "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	for index := uint32(0); index < uint32(acl.AceCount); index++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(acl, index, &ace); err != nil {
			return err
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			return ErrInvalidRequest
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart)).String()
		if sid == "S-1-5-18" || sid == "S-1-5-32-544" || sid == installerSID {
			continue
		}
		if sid == "S-1-3-0" && ace.Header.AceFlags&windows.INHERIT_ONLY_ACE != 0 && ace.Mask == windows.GENERIC_ALL {
			continue
		}
		if sid != "S-1-5-32-545" && sid != "S-1-15-2-1" && sid != "S-1-15-2-2" && sid != ownerSID {
			return ErrInvalidRequest
		}
		if ace.Mask != 0x1200a9 && ace.Mask != windows.GENERIC_READ|windows.GENERIC_EXECUTE {
			return ErrInvalidRequest
		}
	}
	return nil
}

func protectVerifiedWindowsReleasePin(ctx context.Context, path string, source installsource.Source, ownerSID string) (resultErr error) {
	if source.Distribution != installsource.Official {
		return ErrInvalidRequest
	}
	if err := source.Verify(path); err != nil {
		return err
	}
	// PE and platform policy checks open the image; afterwards exclusive handles
	// freeze its identity and the complete hash is checked again before repair.
	if err := verifyWindowsInstalledBinary(ctx, path, source.Architecture); err != nil {
		return err
	}
	release, err := pinWindowsArtifactAncestors(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, release()) }()
	handle, info, err := openWindowsRuntimeObject(path, false)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(handle), path)
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if info.NumberOfLinks != 1 {
		return ErrInvalidRequest
	}
	owner, err := windowsRuntimeTrustedOwner()
	if err != nil {
		return err
	}
	if err := checkWindowsRuntimeHandleSecurity(handle, owner, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;BU)"); err != nil {
		return err
	}
	digest := sha256.New()
	length, err := io.Copy(digest, io.LimitReader(file, source.Length+1))
	if err != nil || length != source.Length || hex.EncodeToString(digest.Sum(nil)) != source.SHA256 {
		return errors.Join(ErrInvalidRequest, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := applyWindowsHandleOwnedDACL(handle, owner, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;"+ownerSID+")"); err != nil {
		return err
	}
	return nil
}
