//go:build windows

package envinject

import (
	"errors"
	"io/fs"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
)

// Windows does not expose meaningful POSIX permission bits. The atomic writer
// protects host ENV state with an exact non-inheriting DACL for the current
// owner, SYSTEM, and Administrators, so reads validate that same boundary.
func secureStateFile(path string, info fs.FileInfo, maximum int64) (ok bool, resultErr error) {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 1 || info.Size() > maximum {
		return false, nil
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, token.Close())
		if resultErr != nil {
			ok = false
		}
	}()
	user, err := token.GetTokenUser()
	if err != nil {
		return false, err
	}
	if user == nil || user.User.Sid == nil || !user.User.Sid.IsValid() {
		return false, nil
	}
	sid := user.User.Sid.String()
	descriptor := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if sid != "S-1-5-18" {
		descriptor += "(A;;FA;;;" + sid + ")"
	}
	return windowssecurity.CheckProtectedDACLMatches(path, descriptor)
}
