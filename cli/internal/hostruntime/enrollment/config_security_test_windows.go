//go:build windows

package enrollment

import (
	"errors"

	"golang.org/x/sys/windows"
)

func makeEnrollmentConfigPrivate(path string) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	sddl := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if user.User.Sid.String() != "S-1-5-18" {
		sddl += "(A;;FA;;;" + user.User.Sid.String() + ")"
	}
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func makeEnrollmentStateRootPrivateForTest(path string) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return errors.New("current Windows token has no valid SID")
	}
	acl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	if user.User.Sid.String() != "S-1-5-18" {
		acl += "(A;OICI;FA;;;" + user.User.Sid.String() + ")"
	}
	descriptor, err := windows.SecurityDescriptorFromString(acl)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func makeEnrollmentStateRootWorldWritableForTest(path string) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return errors.New("current Windows token has no valid SID")
	}
	acl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;WD)"
	if user.User.Sid.String() != "S-1-5-18" {
		acl = "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + user.User.Sid.String() + ")(A;OICI;FA;;;WD)"
	}
	descriptor, err := windows.SecurityDescriptorFromString(acl)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func makeEnrollmentStateRootWorldReadableForTest(path string) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return errors.New("current Windows token has no valid SID")
	}
	acl := "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)"
	if user.User.Sid.String() != "S-1-5-18" {
		acl += "(A;OICI;FA;;;" + user.User.Sid.String() + ")"
	}
	acl += "(A;OICI;GRGX;;;WD)"
	descriptor, err := windows.SecurityDescriptorFromString(acl)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
