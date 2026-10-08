//go:build windows

package windowssecurity

import (
	"errors"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// OwnerFullControlDirectoryDACL is the canonical protected, inheritable DACL
// for per-user Paperboat state that must survive Windows S4U logon sessions.
func OwnerFullControlDirectoryDACL(ownerSID string) string {
	return "D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;FA;;;" + ownerSID + ")"
}

// ProtectedDACLMatches compares a protected file DACL with the requested
// descriptor. Windows may serialize the current elevated administrator SID as
// the well-known LA alias, so compare that canonical representation too. LA
// is accepted only for an elevated token; non-admin users still require their
// exact SID.
func ProtectedDACLMatches(path, expected string) bool {
	matches, err := CheckProtectedDACLMatches(path, expected)
	return err == nil && matches
}

// CheckProtectedDACLMatches distinguishes a policy mismatch from a failed probe.
func CheckProtectedDACLMatches(path, expected string) (bool, error) {
	want, err := windows.SecurityDescriptorFromString(expected)
	if err != nil {
		return false, err
	}
	canonical, err := descriptorSDDL(want)
	if err != nil {
		return false, err
	}
	got, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	if got == nil {
		return false, nil
	}
	return protectedDACLMatches(got, canonical)
}

// ProtectedHandleDACLMatches performs the same exact DACL check through an
// already trusted open handle, so callers do not re-resolve an attacker-
// replaceable path while establishing an object's security.
func ProtectedHandleDACLMatches(handle windows.Handle, expected string) bool {
	matches, err := CheckProtectedHandleDACLMatches(handle, expected)
	return err == nil && matches
}

// CheckProtectedHandleDACLMatches distinguishes native handle probe failures
// from a protected-DACL mismatch without re-resolving an object path.
func CheckProtectedHandleDACLMatches(handle windows.Handle, expected string) (bool, error) {
	got, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	if got == nil {
		return false, nil
	}
	return protectedDACLMatches(got, expected)
}

func protectedDACLMatches(got *windows.SECURITY_DESCRIPTOR, expected string) (matches bool, resultErr error) {
	control, _, err := got.Control()
	if err != nil {
		return false, err
	}
	text, err := descriptorSDDL(got)
	if err != nil {
		return false, err
	}
	actual := dacl(text)
	expectedDACL := dacl(expected)
	// AI records auto-inheritance; it does not leave inheritance enabled.
	expectsAutoInherited := strings.Contains(expected, "D:AI")
	if control&windows.SE_DACL_PROTECTED == 0 && !expectsAutoInherited {
		return false, nil
	}
	if actual == expectedDACL {
		return true, nil
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false, err
	}
	defer func() { resultErr = errors.Join(resultErr, token.Close()) }()
	user, err := token.GetTokenUser()
	if err != nil {
		return false, err
	}
	if user == nil || user.User.Sid == nil {
		return false, nil
	}
	if !strings.HasSuffix(user.User.Sid.String(), "-500") {
		return false, nil
	}
	return actual == dacl(strings.Replace(expected, user.User.Sid.String(), "LA", 1)), nil
}

// x/sys's SECURITY_DESCRIPTOR.String discards this native conversion error.
// Keep its exact conversion flags and release the API-owned output buffer.
var convertDescriptorToString = windows.NewLazySystemDLL("advapi32.dll").NewProc("ConvertSecurityDescriptorToStringSecurityDescriptorW")

func descriptorSDDL(sd *windows.SECURITY_DESCRIPTOR) (value string, resultErr error) {
	var output *uint16
	ok, _, err := convertDescriptorToString.Call(uintptr(unsafe.Pointer(sd)), 1, 0xff, uintptr(unsafe.Pointer(&output)), 0)
	if ok == 0 {
		if err == windows.ERROR_SUCCESS {
			err = windows.ERROR_INVALID_SECURITY_DESCR
		}
		return "", err
	}
	defer func() {
		_, freeErr := windows.LocalFree(windows.Handle(unsafe.Pointer(output)))
		resultErr = errors.Join(resultErr, freeErr)
	}()
	return windows.UTF16PtrToString(output), nil
}

// OwnerMatchesSID rejects attacker-owned filesystem objects even when their
// current DACL text matches the expected protected ACL. A Windows owner can
// restore WRITE_DAC and replace a machine-scope DPAPI credential later.
func OwnerMatchesSID(path string, expected *windows.SID) bool {
	matches, err := CheckOwnerMatchesSID(path, expected)
	return err == nil && matches
}

// CheckOwnerMatchesSID retains the original native lookup/owner probe failure.
func CheckOwnerMatchesSID(path string, expected *windows.SID) (bool, error) {
	if expected == nil || !expected.IsValid() {
		return false, nil
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	if descriptor == nil {
		return false, nil
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	return owner != nil && owner.Equals(expected), nil
}

// HandleOwnerMatchesSID validates ownership without resolving a filesystem
// path after the object has been opened.
func HandleOwnerMatchesSID(handle windows.Handle, expected *windows.SID) bool {
	matches, err := CheckHandleOwnerMatchesSID(handle, expected)
	return err == nil && matches
}

// CheckHandleOwnerMatchesSID preserves security/owner lookup failures on an
// already trusted handle. The boolean adapter remains fail closed.
func CheckHandleOwnerMatchesSID(handle windows.Handle, expected *windows.SID) (bool, error) {
	if expected == nil || !expected.IsValid() {
		return false, nil
	}
	descriptor, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	if descriptor == nil {
		return false, nil
	}
	owner, _, err := descriptor.Owner()
	if err != nil {
		return false, err
	}
	return owner != nil && owner.Equals(expected), nil
}

func dacl(value string) string {
	start := strings.Index(value, "D:")
	if start < 0 {
		return ""
	}
	open := strings.IndexByte(value[start:], '(')
	if open < 0 {
		return ""
	}
	body := value[start+open:]
	var result strings.Builder
	seen := make(map[string]struct{})
	for len(body) > 0 {
		begin := strings.IndexByte(body, '(')
		if begin < 0 {
			break
		}
		end := strings.IndexByte(body[begin:], ')')
		if end < 0 {
			return ""
		}
		ace := body[begin : begin+end+1]
		ace = strings.ReplaceAll(ace, "S-1-5-18", "SY")
		if _, ok := seen[ace]; !ok {
			seen[ace] = struct{}{}
			result.WriteString(ace)
		}
		body = body[begin+end+1:]
	}
	return "D:" + result.String()
}
