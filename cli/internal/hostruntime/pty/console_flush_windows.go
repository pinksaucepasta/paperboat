//go:build windows

package pty

import (
	"context"
	"fmt"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// FlushConsoleInputArgs is the command line of the helper Interrupt starts inside the pseudoconsole (pb daemon
// __flush-console-input, daemoncmd). Only a process attached to the console can open its CONIN$.
var FlushConsoleInputArgs = []string{"daemon", "__flush-console-input"}

// FlushConsoleInput discards the input records queued in the console this process is attached to.
func FlushConsoleInput() error {
	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return fmt.Errorf("open console input: %w", err)
	}
	defer windows.CloseHandle(handle)
	return flushConsoleInputBuffer(handle)
}

var flushConsoleInputBufferProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("FlushConsoleInputBuffer")

func flushConsoleInputBuffer(handle windows.Handle) error {
	if r, _, err := flushConsoleInputBufferProc.Call(uintptr(handle)); r == 0 {
		return fmt.Errorf("flush console input: %w", err)
	}
	return nil
}

// flushQueuedInput is the Windows counterpart of the unix flushPendingInput: bytes conhost already accepted from a paste the
// shell never read would otherwise sit in front of Ctrl-C and reach the shell after it. ConPTY has no flush call for its owner, so
// a short-lived helper is started inside the same pseudoconsole (CreateProcess with the pseudoconsole attribute, which never
// touches the input pipe) and flushes its CONIN$. Measured on Windows 11: 1048574 queued events before, 0 after, helper
// start 60 ms. The helper never outlives timeout.
func (p *Process) flushQueuedInput(timeout time.Duration) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	commandLine, err := commandLine(executable, FlushConsoleInputArgs)
	if err != nil {
		return err
	}
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return err
	}
	defer attributes.Delete()
	console := p.console
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&console)), unsafe.Sizeof(console)); err != nil {
		return err
	}
	startup := windows.StartupInfoEx{}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Flags = windows.STARTF_USESTDHANDLES
	startup.ProcThreadAttributeList = attributes.List()
	var info windows.ProcessInformation
	flags := uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := windows.CreateProcess(nil, &commandLine[0], nil, nil, false, flags, nil, nil, &startup.StartupInfo, &info); err != nil {
		return fmt.Errorf("start console input flush: %w", err)
	}
	windows.CloseHandle(info.Thread)
	defer windows.CloseHandle(info.Process)
	event, err := windows.WaitForSingleObject(info.Process, uint32(timeout/time.Millisecond))
	if err != nil {
		return err
	}
	if event != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(info.Process, 1)
		return context.DeadlineExceeded
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.Process, &code); err != nil || code != 0 {
		return fmt.Errorf("console input flush exited with %d: %v", code, err)
	}
	return nil
}
