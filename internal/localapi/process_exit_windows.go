//go:build windows

package localapi

import (
	"context"
	"errors"

	"golang.org/x/sys/windows"
)

func watchProcessExit(pid int) (<-chan struct{}, func()) {
	done := make(chan struct{})
	if pid <= 0 {
		return done, func() {}
	}
	process, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		// Access to query a same-user client can still be denied across Windows
		// service/session boundaries. That means the optional lifetime watcher is
		// unavailable, not that the client exited. Only an invalid PID proves the
		// process is already gone; the pipe hangup remains the cleanup boundary in
		// every other failure case.
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			close(done)
		}
		return done, func() {}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		defer windows.CloseHandle(process)
		for {
			state, waitErr := windows.WaitForSingleObject(process, 250)
			if waitErr != nil {
				return
			}
			if state == windows.WAIT_OBJECT_0 {
				close(done)
				return
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
		}
	}()
	return done, cancel
}
