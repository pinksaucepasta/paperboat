//go:build windows

package windowssecurity

import (
	"path/filepath"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

// A file created with an explicit owner and protected DACL matches the descriptor string it was created from, also
// when that string carries an owner prefix and the user is the built-in Administrator (RID 500, which the API
// writes as "LA"): the CI runner's user is one.
func TestProtectedHandleDACLMatchesADescriptorWithAnOwnerPrefix(t *testing.T) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := user.User.Sid.String()
	descriptor := "O:" + sid + "D:P(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;" + sid + ")"
	sd, err := windows.SecurityDescriptorFromString(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(filepath.Join(t.TempDir(), "f"))
	if err != nil {
		t.Fatal(err)
	}
	attributes := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE|windows.READ_CONTROL, 0, &attributes, windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
	runtime.KeepAlive(sd)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	if !HandleOwnerMatchesSID(handle, user.User.Sid) {
		t.Skip("the token cannot own a file as itself; nothing to compare")
	}
	if !ProtectedHandleDACLMatches(handle, descriptor) {
		t.Fatalf("the DACL created from %s does not match it", descriptor)
	}
}
