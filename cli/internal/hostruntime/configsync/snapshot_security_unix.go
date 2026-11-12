//go:build unix

package configsync

import (
	"io/fs"
	"os"
)

// platformHasExecBits is false where files carry no POSIX executable bit (Windows): the executable intent then comes from the repository.
const platformHasExecBits = true

func safeSnapshotPermissions(_ string, info fs.FileInfo) bool { return info.Mode().Perm()&0o002 == 0 }
func snapshotFileMode(info fs.FileInfo) os.FileMode           { return info.Mode().Perm() }
