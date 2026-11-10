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

// queryHelperFlags: the helper runs 10-40 ms inside the session's console, which can receive a real Ctrl+C now that
// session shells no longer inherit the host's ignore flag (ctrlc_windows.go). CREATE_NEW_PROCESS_GROUP makes the helper
// ignore it, so a Ctrl+C typed meanwhile cannot kill it and read as a failed probe.
const queryHelperFlags = uint32(windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_NEW_PROCESS_GROUP)

// QueryConsoleModeArgs is the command line of the helper Interrupt starts inside the pseudoconsole (pb daemon
// __query-console-mode, daemoncmd). Only a process attached to the console can open its CONIN$, and the mode it reads
// is the one conhost itself uses to decide whether a 0x03 is a Ctrl-C signal or data (measured on Windows 11).
var QueryConsoleModeArgs = []string{"daemon", "__query-console-mode"}

// The helper's exit codes: it prints nothing (its output would land in the session).
const (
	ExitInputProcessed = 10 // ENABLE_PROCESSED_INPUT set: 0x03 is the Ctrl-C signal
	ExitInputRaw       = 11 // clear: 0x03 is data for the program (vim-like raw mode, PSReadLine mid-edit)
)

// QueryConsoleInputProcessed reads the input mode of the console this process is attached to. Read-only: it never
// sets a mode or flushes input (the reverted flush helper broke Ctrl-C delivery).
func QueryConsoleInputProcessed() (bool, error) {
	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		return false, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return false, fmt.Errorf("open console input: %w", err)
	}
	defer windows.CloseHandle(handle)
	var mode uint32
	if err := windows.GetConsoleMode(handle, &mode); err != nil {
		return false, fmt.Errorf("read console input mode: %w", err)
	}
	return mode&windows.ENABLE_PROCESSED_INPUT != 0, nil
}

// inputIsProcessed asks the session's console whether Ctrl-C is a signal, like the unix twin's ISIG check
// (pty_unix.go Interrupt): a helper is started inside the same pseudoconsole (CreateProcess with the pseudoconsole
// attribute, which never touches the input pipe) and reports through its exit code. Measured 10-40 ms. An error means
// unknown; the caller then treats the key as the interrupt, as before.
func (p *Process) inputIsProcessed(timeout time.Duration) (bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return false, err
	}
	commandLine, err := commandLine(executable, QueryConsoleModeArgs)
	if err != nil {
		return false, err
	}
	attributes, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return false, err
	}
	defer attributes.Delete()
	console := p.console
	if err := attributes.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&console)), unsafe.Sizeof(console)); err != nil {
		return false, err
	}
	startup := windows.StartupInfoEx{}
	startup.Cb = uint32(unsafe.Sizeof(startup))
	startup.Flags = windows.STARTF_USESTDHANDLES
	startup.ProcThreadAttributeList = attributes.List()
	var info windows.ProcessInformation
	flags := queryHelperFlags
	// hidden: attached to the session's pseudoconsole through the attribute above, so it never gets a console window.
	if err := windows.CreateProcess(nil, &commandLine[0], nil, nil, false, flags, nil, nil, &startup.StartupInfo, &info); err != nil {
		return false, fmt.Errorf("start console mode query: %w", err)
	}
	windows.CloseHandle(info.Thread)
	defer windows.CloseHandle(info.Process)
	event, err := windows.WaitForSingleObject(info.Process, uint32(timeout/time.Millisecond))
	if err != nil {
		return false, err
	}
	if event != windows.WAIT_OBJECT_0 {
		_ = windows.TerminateProcess(info.Process, 1)
		return false, context.DeadlineExceeded
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.Process, &code); err != nil {
		return false, err
	}
	switch code {
	case ExitInputProcessed:
		return true, nil
	case ExitInputRaw:
		return false, nil
	}
	return false, fmt.Errorf("console mode query exited with %d", code)
}
