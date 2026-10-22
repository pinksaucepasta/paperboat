//go:build windows

package atomicfiletest

import (
	"errors"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/windowssecurity"
)

func checkOwnerOnly(path string, _ os.FileMode) error {
	descriptor, err := atomicfile.DefaultSecurityDescriptor()
	if err != nil {
		return err
	}
	matches, err := windowssecurity.CheckProtectedDACLMatches(path, descriptor)
	if err != nil {
		return err
	}
	if !matches {
		return errors.New("DACL is not the protected owner/SYSTEM/Administrators descriptor atomicfile.Write applies")
	}
	return nil
}
