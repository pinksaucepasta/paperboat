//go:build windows

package bugreport

import (
	"context"
	"errors"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var recordingKernel = windows.NewLazySystemDLL("kernel32.dll")
var recordingPeekPipe = recordingKernel.NewProc("PeekNamedPipe")
var recordingReadConsole = recordingKernel.NewProc("ReadConsoleInputW")

type recordingConsoleEvent struct {
	Kind    uint16
	Padding uint16
	Data    [16]byte
}
type recordingKeyEvent struct {
	Down       int32
	Repeat     uint16
	VirtualKey uint16
	Scan       uint16
	Character  uint16
	Control    uint32
}

func waitRecordingFile(ctx context.Context, file *os.File) error {
	handle := windows.Handle(file.Fd())
	kind, err := windows.GetFileType(handle)
	if err != nil {
		return recordingInputError{Err: err}
	}
	if kind != windows.FILE_TYPE_CHAR && kind != windows.FILE_TYPE_PIPE {
		return errRecordingInput
	}
	if kind == windows.FILE_TYPE_CHAR {
		var mode uint32
		if err := windows.GetConsoleMode(handle, &mode); err != nil {
			return recordingInputError{Err: err}
		}
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if kind == windows.FILE_TYPE_CHAR {
			var count uint32
			if err := windows.GetNumberOfConsoleInputEvents(handle, &count); err != nil {
				return recordingInputError{Err: err}
			}
			if count > 0 {
				var event recordingConsoleEvent
				var read uint32
				result, _, err := recordingReadConsole.Call(uintptr(handle), uintptr(unsafe.Pointer(&event)), 1, uintptr(unsafe.Pointer(&read)))
				if result == 0 {
					return recordingInputError{Err: recordingNativeError(err)}
				}
				key := (*recordingKeyEvent)(unsafe.Pointer(&event.Data[0]))
				if read == 1 && event.Kind == 1 && key.Down != 0 && key.VirtualKey == 0x0d {
					return nil
				}
				continue
			}
		} else {
			var available uint32
			result, _, err := recordingPeekPipe.Call(uintptr(handle), 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0)
			if result == 0 {
				if errors.Is(err, windows.ERROR_BROKEN_PIPE) {
					return nil
				}
				return recordingInputError{Err: recordingNativeError(err)}
			}
			if available > 0 {
				var value [1]byte
				count, err := file.Read(value[:])
				if count > 0 && value[0] == '\n' {
					return nil
				}
				if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, io.EOF) {
					return nil
				}
				if count > 0 && errors.Is(err, windows.ERROR_MORE_DATA) {
					continue
				}
				if err != nil {
					return recordingInputError{Err: err}
				}
				if count == 0 || value[0] == '\n' {
					return nil
				}
				continue
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func recordingNativeError(err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return windows.ERROR_INVALID_HANDLE
	}
	return err
}
