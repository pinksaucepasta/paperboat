//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"io"
	"os"
)

func readExecInputFile(ctx context.Context, file *os.File, value []byte) (int, error) {
	raw, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		n, ready := 0, false
		var readErr error
		err := raw.Control(func(fd uintptr) {
			polls := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
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
			if polls[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
				ready = true
				n, readErr = unix.Read(int(fd), value)
			}
		})
		if err != nil {
			return 0, err
		}
		if errors.Is(readErr, unix.EINTR) || errors.Is(readErr, unix.EAGAIN) {
			continue
		}
		if readErr != nil {
			return n, readErr
		}
		if ready {
			if n == 0 {
				return 0, io.EOF
			}
			return n, nil
		}
	}
}
