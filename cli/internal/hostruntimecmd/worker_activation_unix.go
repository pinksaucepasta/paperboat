//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
)

func readWorkerActivationByte(ctx context.Context, file *os.File) (byte, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var value [1]byte
	for {
		if err := runtimeContextFailure(ctx); err != nil {
			return 0, err
		}
		count, ready := 0, false
		var readErr error
		controlErr := raw.Control(func(fd uintptr) {
			polls := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
			_, readErr = unix.Poll(polls, 20)
			if readErr != nil {
				return
			}
			if readErr = runtimeContextFailure(ctx); readErr != nil {
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
			count, readErr = unix.Read(int(fd), value[:])
		})
		if controlErr != nil {
			return 0, controlErr
		}
		if errors.Is(readErr, unix.EINTR) || errors.Is(readErr, unix.EAGAIN) {
			continue
		}
		if readErr != nil {
			return 0, readErr
		}
		if ready {
			if count == 0 {
				return 0, io.EOF
			}
			return value[0], nil
		}
	}
}
