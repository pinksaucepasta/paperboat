//go:build linux || darwin

package runtime

import (
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"os"
)

func validateIndependentAccount(d sessionowner.Descriptor) error {
	if d.UID != os.Geteuid() || d.GID != os.Getegid() || d.OwnerSID != "" {
		return sessionowner.ErrInvalid
	}
	return nil
}
