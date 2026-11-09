//go:build !windows

package atomicfile

import "syscall"

// ErrNotDirectory is the cause of a path that runs through a file.
const ErrNotDirectory = syscall.ENOTDIR
