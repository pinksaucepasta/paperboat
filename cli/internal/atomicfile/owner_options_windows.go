//go:build windows

package atomicfile

import "os"

// CurrentOwnerOptions returns the owner policy for files owned by the current process. Windows has no POSIX UID/GID ownership. The Windows writer applies its
// protected current-user/SYSTEM/Administrators ACL instead.
func CurrentOwnerOptions(mode os.FileMode) Options {
	return Options{Mode: mode, OwnerUID: -1, OwnerGID: -1}
}
