//go:build windows

package session

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows console input is delivered by the console handle itself. The
// ConPTY implementation owns cancellation of that handle; keep this helper
// honest by checking cancellation before the blocking read rather than
// claiming Unix poll semantics on Windows.
func readLocalInput(ctx context.Context, file *os.File, p []byte) (int, error) {
	if ctx == nil || file == nil {
		return 0, os.ErrInvalid
	}
	if err := context.Cause(ctx); err != nil {
		return 0, err
	}
	handle := windows.Handle(file.Fd())
	stop := context.AfterFunc(ctx, func() {
		// Windows ReadFile on console and pipe handles does not observe Go
		// context cancellation. Cancel the outstanding operation without
		// closing the process-owned stdin handle so repeated sessions remain
		// usable.
		_ = windows.CancelIoEx(handle, nil)
	})
	n, err := readHandle(handle, file, p)
	stop()
	if cause := context.Cause(ctx); cause != nil && (err == nil || errors.Is(err, windows.ERROR_OPERATION_ABORTED)) {
		return n, cause
	}
	return n, err
}

// consoleInput carries what a console read leaves behind between calls (a split surrogate pair, bytes that did not fit).
var consoleInput struct {
	sync.Mutex
	decoder consoleDecoder
}

// readHandle reads a console with ReadConsoleW: ReadFile on a console handle ends the stream at Ctrl-Z (0x1a) even in raw
// VT mode and swallows the bytes after it, so job control from a Windows client never reached the remote shell
// (measured through ssh -tt/ConPTY on Windows 11; ReadConsoleW with a zero wakeup mask delivers 0x1a intact). Anything that
// is not a console (a pipe, a file) keeps ReadFile.
func readHandle(handle windows.Handle, file *os.File, p []byte) (int, error) {
	if len(p) == 0 { // nothing to fill: the read loop below would drain the console forever
		return 0, nil
	}
	var mode uint32
	if windows.GetConsoleMode(handle, &mode) != nil {
		return file.Read(p)
	}
	consoleInput.Lock()
	if consoleInput.decoder.pending() {
		n := consoleInput.decoder.decode(p, nil)
		consoleInput.Unlock()
		return n, nil
	}
	consoleInput.Unlock()
	buffer := make([]uint16, min(max(len(p)/3, 1), 10922))
	control := struct{ length, initial, mask, state uint32 }{length: 16}
	for {
		var read uint32
		if err := windows.ReadConsole(handle, &buffer[0], uint32(len(buffer)), &read, (*byte)(unsafe.Pointer(&control))); err != nil {
			return 0, err
		}
		if read == 0 {
			return 0, io.EOF
		}
		consoleInput.Lock()
		n := consoleInput.decoder.decode(p, buffer[:read])
		consoleInput.Unlock()
		if n > 0 { // a lone high surrogate yields nothing yet: read its other half
			return n, nil
		}
	}
}
