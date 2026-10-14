//go:build windows

package enrollment

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
	"golang.org/x/sys/windows"
	"os"
)

func secureEnrollmentConfigFile(path string, info os.FileInfo, maximum int64) bool {
	matches, err := checkEnrollmentConfigFile(path, info, maximum)
	return err == nil && matches
}

func checkEnrollmentConfigFile(path string, info os.FileInfo, maximum int64) (matches bool, resultErr error) {
	if info == nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maximum {
		return false, nil
	}
	single, err := checkWindowsSingleLink(path)
	if err != nil || !single {
		return false, err
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return false, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, token.Close())
		if resultErr != nil {
			matches = false
		}
	}()
	user, err := token.GetTokenUser()
	if err != nil {
		return false, err
	}
	if user == nil || user.User.Sid == nil {
		return false, nil
	}
	descriptor := "D:P(A;;FA;;;SY)(A;;FA;;;BA)"
	if user.User.Sid.String() != "S-1-5-18" {
		descriptor += "(A;;FA;;;" + user.User.Sid.String() + ")"
	}
	return windowssecurity.CheckProtectedDACLMatches(path, descriptor)
}
