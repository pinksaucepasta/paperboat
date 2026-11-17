//go:build windows

package main

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/protocol"
	"golang.org/x/sys/windows"
	"io"
	"os"
	"syscall"
	"time"
	"unsafe"
)

var execInputKernel = windows.NewLazySystemDLL("kernel32.dll")
var execPeekPipe = execInputKernel.NewProc("PeekNamedPipe")
var execPeekConsole = execInputKernel.NewProc("PeekConsoleInputW")

type execConsoleEvent struct {
	Kind    uint16
	Padding uint16
	Data    [16]byte
}
type execConsoleKey struct {
	Down                                int32
	Repeat, VirtualKey, Scan, Character uint16
	Control                             uint32
}

func readExecInputFile(ctx context.Context, file *os.File, value []byte) (int, error) {
	handle := windows.Handle(file.Fd())
	kind, err := windows.GetFileType(handle)
	if err != nil {
		return 0, err
	}
	var mode uint32
	if kind == windows.FILE_TYPE_CHAR {
		if err := windows.GetConsoleMode(handle, &mode); err != nil {
			return file.Read(value) // a character device that is not a console is NUL: an immediate EOF
		}
	} else if kind != windows.FILE_TYPE_PIPE {
		return 0, os.ErrInvalid
	}
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		ready := false
		if kind == windows.FILE_TYPE_PIPE {
			available, err := execPipeAvailable(handle)
			if err != nil {
				return 0, err
			}
			ready = available > 0
		} else {
			var count uint32
			if err := windows.GetNumberOfConsoleInputEvents(handle, &count); err != nil {
				return 0, err
			}
			// Bound private inspection memory by the existing terminal-frame budget.
			// Reject before consuming even one record; pasted input remains caller-owned.
			maximum := uint32(protocol.MaxBinaryFrame / int(unsafe.Sizeof(execConsoleEvent{})))
			if count > maximum {
				return 0, errExecInputLimit
			}
			if count > 0 {
				events := make([]execConsoleEvent, count)
				var peeked uint32
				result, _, err := execPeekConsole.Call(uintptr(handle), uintptr(unsafe.Pointer(&events[0])), uintptr(count), uintptr(unsafe.Pointer(&peeked)))
				if result == 0 {
					return 0, execInputNativeError(err)
				}
				ready = execConsoleByteReady(events[:peeked], mode)
			}
		}
		if ready {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
			n, err := file.Read(value)
			if n > 0 && errors.Is(err, windows.ERROR_MORE_DATA) {
				err = nil
			}
			if n > 0 && err == nil && kind == windows.FILE_TYPE_PIPE {
				n = execCoalescePipe(handle, file, value, n)
			}
			return n, err
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-ticker.C:
		}
	}
}

// execPipeAvailable is the number of bytes a pipe holds now; a closed writer is io.EOF.
func execPipeAvailable(handle windows.Handle) (uint32, error) {
	var available uint32
	result, _, err := execPeekPipe.Call(uintptr(handle), 0, 0, 0, uintptr(unsafe.Pointer(&available)), 0)
	if result == 0 {
		if errors.Is(err, windows.ERROR_BROKEN_PIPE) {
			return 0, io.EOF
		}
		return 0, execInputNativeError(err)
	}
	return available, nil
}

// execCoalescePipe keeps reading what the pipe already holds, or receives within ~2 ms, into value after its first n
// bytes. A Windows pipe hands over at most its buffer (often 4 KiB) per read, so without this every stdin frame was a
// few KiB: 10x the packets of file input and little for zstd to work with. It never blocks: it reads only bytes the
// pipe reports, and an error or EOF is left for the next read.
func execCoalescePipe(handle windows.Handle, file *os.File, value []byte, n int) int {
	for idle := 0; n < len(value) && idle < 2; {
		available, err := execPipeAvailable(handle)
		if err != nil {
			break
		}
		if available == 0 {
			idle++
			//paperboat:allow-source-policy sleep owner=exec-input reason=bounded-pipe-coalesce-pacing
			time.Sleep(time.Millisecond)
			continue
		}
		idle = 0
		read, err := file.Read(value[n : n+min(int(available), len(value)-n)])
		if read > 0 {
			n += read
		}
		if err != nil && !errors.Is(err, windows.ERROR_MORE_DATA) {
			break
		}
	}
	return n
}

func execConsoleByteReady(events []execConsoleEvent, mode uint32) bool {
	highSurrogate := false
	for _, event := range events {
		if event.Kind != 1 {
			continue
		}
		key := (*execConsoleKey)(unsafe.Pointer(&event.Data[0]))
		if key.Down == 0 {
			continue
		}
		if mode&windows.ENABLE_LINE_INPUT != 0 {
			if key.VirtualKey == 0x0d {
				return true
			}
			continue
		}
		if key.Character >= 0xd800 && key.Character <= 0xdbff {
			highSurrogate = true
			continue
		}
		if key.Character >= 0xdc00 && key.Character <= 0xdfff {
			if highSurrogate {
				return true
			}
			continue
		}
		if key.Character == 3 && mode&windows.ENABLE_PROCESSED_INPUT != 0 {
			continue
		}
		if key.Character != 0 {
			return true
		}
		if mode&windows.ENABLE_VIRTUAL_TERMINAL_INPUT != 0 {
			switch key.VirtualKey {
			case 0x21, 0x22, 0x23, 0x24, 0x25, 0x26, 0x27, 0x28, 0x2d, 0x2e:
				return true
			}
			if key.VirtualKey >= 0x70 && key.VirtualKey <= 0x87 {
				return true
			}
		}
	}
	return false
}
func execInputNativeError(err error) error {
	if err == nil || errors.Is(err, syscall.Errno(0)) {
		return windows.ERROR_INVALID_HANDLE
	}
	return err
}
