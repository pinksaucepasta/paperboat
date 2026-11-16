//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"time"
	"unsafe"
)

var workerPeekPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

func readWorkerActivationByte(ctx context.Context, file *os.File) (byte, error) {
	handle := windows.Handle(file.Fd())
	kind, err := windows.GetFileType(handle)
	if err != nil {
		return 0, err
	}
	if kind != windows.FILE_TYPE_PIPE {
		return 0, runtimeFailure("worker activation requires the supervisor pipe")
	}
	// The supervisor pipe is polled (a blocking ReadFile cannot be cancelled by ctx). The first seconds poll fast
	// for the activation line; a worker that then waits for its next control command polls every
	// workerControlIdlePoll: a 20 ms poll for the worker's whole life was ~50 wakeups/s and ~1% of a core idle (PR-03).
	started := time.Now()
	timer := time.NewTimer(workerControlFastPoll)
	defer timer.Stop()
	for {
		if err := runtimeContextFailure(ctx); err != nil {
			return 0, err
		}
		var available uint32
		result, _, err := workerPeekPipe.Call(uintptr(handle), 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0)
		if result == 0 {
			if errors.Is(err, windows.ERROR_BROKEN_PIPE) {
				return 0, io.EOF
			}
			if err == nil || err == windows.ERROR_SUCCESS {
				err = windows.ERROR_INVALID_HANDLE
			}
			return 0, err
		}
		if available > 0 {
			var b [1]byte
			_, err := io.ReadFull(file, b[:])
			return b[0], err
		}
		delay := workerControlFastPoll
		if time.Since(started) > workerControlFastWindow {
			delay = workerControlIdlePoll
		}
		timer.Reset(delay)
		select {
		case <-ctx.Done():
			return 0, runtimeContextFailure(ctx)
		case <-timer.C:
		}
	}
}

const (
	workerControlFastPoll   = 20 * time.Millisecond
	workerControlFastWindow = 2 * time.Second
	workerControlIdlePoll   = 250 * time.Millisecond
)
