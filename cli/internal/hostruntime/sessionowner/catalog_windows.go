//go:build windows

package sessionowner

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
	"path/filepath"
)

func rejectCatalogReparse(path string) error {
	for current := path; ; current = filepath.Dir(current) {
		attrs, err := windows.GetFileAttributes(windows.StringToUTF16Ptr(current))
		if err != nil {
			return catalogFailure(err)
		}
		if attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return ErrInvalid
		}
		if filepath.Dir(current) == current {
			break
		}
	}
	return nil
}
func catalogDirectoryDACL(sid string) string {
	return "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1200a9;;;" + sid + ")"
}
func checkCatalogRoot(path, sid string) error {
	if sid == "" {
		return ErrInvalid
	}
	if err := rejectCatalogReparse(path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return ErrInvalid
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	owner, err := windowssecurity.CheckOwnerMatchesSID(path, system)
	if err != nil {
		return err
	}
	acl, err := windowssecurity.CheckProtectedDACLMatches(path, catalogDirectoryDACL(sid))
	if err != nil {
		return err
	}
	if !owner || !acl {
		return ErrInvalid
	}
	return nil
}
func setCatalogSecurity(path, sddl string) error {
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windowssecurity.WithRestorePrivilege(func() error {
		return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, owner, nil, dacl, nil)
	})
}
func prepareCatalogRoot(path string, d Descriptor) error {
	if err := checkCatalogRoot(filepath.Dir(path), d.OwnerSID); err != nil {
		return err
	}
	created := false
	if err := os.Mkdir(path, 0700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	if !created {
		return checkCatalogRoot(path, d.OwnerSID)
	}
	if err := rejectCatalogReparse(path); err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	owner, err := windowssecurity.CheckOwnerMatchesSID(path, system)
	if err != nil {
		return err
	}
	if !owner {
		admins, e := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		if e != nil {
			return e
		}
		administrative, e := windowssecurity.CheckOwnerMatchesSID(path, admins)
		if e != nil {
			return e
		}
		if !administrative {
			return ErrInvalid
		}
	}
	if err := setCatalogSecurity(path, "O:SY"+catalogDirectoryDACL(d.OwnerSID)); err != nil {
		return err
	}
	return checkCatalogRoot(path, d.OwnerSID)
}
func writeCatalogDescriptor(path string, body []byte, d Descriptor) error {
	return windowssecurity.WithRestorePrivilege(func() error {
		return atomicfile.Write(path, body, atomicfile.Options{Mode: 0644, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;" + d.OwnerSID + ")"})
	})
}
func verifyCatalogExecutable(d Descriptor) error {
	if err := rejectCatalogReparse(d.Executable); err != nil {
		return err
	}
	system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return err
	}
	owner, err := windowssecurity.CheckOwnerMatchesSID(d.Executable, system)
	if err != nil {
		return err
	}
	acl, err := windowssecurity.CheckProtectedDACLMatches(d.Executable, "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;0x1200a9;;;"+d.OwnerSID+")")
	if err != nil {
		return err
	}
	if !owner || !acl {
		return ErrInvalid
	}
	return d.Source.Verify(d.Executable)
}
func prepareCatalogCapability(d Descriptor) error {
	if err := rejectCatalogReparse(d.RuntimeRoot); err != nil {
		return err
	}
	sid, err := windows.StringToSid(d.OwnerSID)
	if err != nil {
		return err
	}
	owner, err := windowssecurity.CheckOwnerMatchesSID(d.RuntimeRoot, sid)
	if err != nil {
		return err
	}
	acl, err := windowssecurity.CheckProtectedDACLMatches(d.RuntimeRoot, windowssecurity.OwnerFullControlDirectoryDACL(d.OwnerSID))
	if err != nil {
		return err
	}
	if !owner || !acl {
		return ErrInvalid
	}

	for _, path := range []string{filepath.Dir(d.PrivateRoot()), d.PrivateRoot()} {
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		if err := rejectCatalogReparse(path); err != nil {
			return err
		}
		sid, err := windows.StringToSid(d.OwnerSID)
		if err != nil {
			return err
		}
		current, err := windowssecurity.CheckOwnerMatchesSID(path, sid)
		if err != nil {
			return err
		}
		system, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
		if err != nil {
			return err
		}
		privileged, err := windowssecurity.CheckOwnerMatchesSID(path, system)
		if err != nil {
			return err
		}
		admins, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		if err != nil {
			return err
		}
		administrative, err := windowssecurity.CheckOwnerMatchesSID(path, admins)
		if err != nil {
			return err
		}
		if !current && !privileged && !administrative {
			return ErrInvalid
		}
		if err := setCatalogSecurity(path, "O:"+d.OwnerSID+windowssecurity.OwnerFullControlDirectoryDACL(d.OwnerSID)); err != nil {
			return err
		}
	}
	private := "O:" + d.OwnerSID + "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + d.OwnerSID + ")"
	for _, path := range []string{d.TokenPath(), filepath.Join(d.PrivateRoot(), "owner.lock")} {
		if _, err := os.Lstat(path); err == nil {
			file, err := openPrivate(path, false)
			if err != nil {
				return err
			}
			validation := validatePrivate(file, path, d)
			closeErr := file.Close()
			if err := errors.Join(validation, closeErr); err != nil {
				return err
			}
			if path == d.TokenPath() {
				if _, err := ReadCapability(d); err != nil {
					return err
				}
			}
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		var body []byte
		if path == d.TokenPath() {
			var err error
			body, err = newCapability()
			if err != nil {
				return err
			}
		}
		if err := windowssecurity.WithRestorePrivilege(func() error {
			return atomicfile.Write(path, body, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: private})
		}); err != nil {
			return err
		}
	}
	return nil
}
