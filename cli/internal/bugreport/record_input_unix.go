//go:build darwin || linux

package bugreport

import (
	"context"
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

func waitRecordingFile(ctx context.Context, file *os.File) error {
	// File.Fd changes Go-owned nonblocking descriptors to blocking mode. Borrow
	// through SyscallConn instead, keeping poll/read inside its validity window.
	raw, err := file.SyscallConn()
	if err != nil {
		return recordingInputError{Err: err}
	}
	var value [1]byte
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		count, ready := 0, false
		var readErr error
		controlErr := raw.Control(func(descriptor uintptr) {
			polls := []unix.PollFd{{Fd: int32(descriptor), Events: unix.POLLIN}}
			_, readErr = unix.Poll(polls, 20)
			if readErr != nil {
				return
			}
			if readErr = ctx.Err(); readErr != nil {
				return
			}
			if polls[0].Revents&unix.POLLNVAL != 0 {
				readErr = unix.EBADF
				return
			}
			if polls[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) == 0 {
				return
			}
			ready = true
			count, readErr = unix.Read(int(descriptor), value[:])
		})
		if controlErr != nil {
			return recordingInputError{Err: controlErr}
		}
		if errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded) {
			return readErr
		}
		if errors.Is(readErr, unix.EINTR) || errors.Is(readErr, unix.EAGAIN) {
			continue
		}
		if readErr != nil {
			return recordingInputError{Err: readErr}
		}
		if ready && (count == 0 || value[0] == '\n') {
			return nil
		}
	}
}
