//go:build windows

package splitdns

import (
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"golang.org/x/sys/windows"
	"os"
)

func writeCAState(path string, data []byte, mode os.FileMode) error {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	options := atomicfile.Options{Mode: mode, OwnerUID: -1, OwnerGID: -1}
	if user.User.Sid.String() == "S-1-5-18" {
		options.SecurityDescriptor = "O:SYD:P(A;;FA;;;SY)(A;;FA;;;BA)"
	} else if token.IsElevated() {
		options.SecurityDescriptor = "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"
	}
	return atomicfile.Write(path, data, options)
}
