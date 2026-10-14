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
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
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
		select {
		case <-ctx.Done():
			return 0, runtimeContextFailure(ctx)
		case <-ticker.C:
		}
	}
}
