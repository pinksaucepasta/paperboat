//go:build unix

package configsync

import (
	"io/fs"
	"os"
)

// platformHasExecBits is false where files carry no POSIX executable bit (Windows): the executable intent then comes from the repository.
const platformHasExecBits = true

// unsafeSnapshotReason is the skip reason for a file the worker must not sync, or "" when it is safe to read.
func unsafeSnapshotReason(_ string, info fs.FileInfo) string {
	if info.Mode().Perm()&0o002 != 0 {
		return "unsafe_permissions"
	}
	return ""
}

func snapshotFileMode(info fs.FileInfo) os.FileMode { return info.Mode().Perm() }
