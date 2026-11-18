//go:build linux

package execprocess

import (
	"os"

	"golang.org/x/sys/unix"
)

// enlargePipe asks the kernel for a bigger buffer on a child's output pipe. A read returns at most what the pipe holds,
// and the default 64 KiB capped every chunk of a fast writer (`dd`, `cat bigfile`) at 64 KiB however large the read
// buffer was. It is best effort: the unprivileged limit is /proc/sys/fs/pipe-max-size, and a refusal only keeps the default.
func enlargePipe(reader any, size int) {
	if file, ok := reader.(*os.File); ok {
		_, _ = unix.FcntlInt(file.Fd(), unix.F_SETPIPE_SZ, size)
	}
}

// pipeSize reports a pipe's capacity (tests).
func pipeSize(file *os.File) int {
	size, err := unix.FcntlInt(file.Fd(), unix.F_GETPIPE_SZ, 0)
	if err != nil {
		return 0
	}
	return size
}
