//go:build windows

package session

import (
	"os"

	"golang.org/x/sys/windows"
)

// EnableVirtualTerminalOutput turns on VT processing for stdout and returns the call that puts the old mode back. A legacy
// conhost window (cmd.exe started by conhost.exe) leaves it off, so the status bar and the remote shell's colours and cursor
// moves printed as literal "←[...m" text (X-24, found in a real console window); Windows Terminal and ConPTY have it on.
// Docker's CLI does the same (moby/term EnableVirtualTerminalProcessing).
func EnableVirtualTerminalOutput() (restore func()) {
	return enableVirtualTerminalOutput(windows.Handle(os.Stdout.Fd()))
}

func enableVirtualTerminalOutput(handle windows.Handle) func() {
	var mode uint32
	if windows.GetConsoleMode(handle, &mode) != nil || mode&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		return func() {} // not a console (a pipe or file), or already on
	}
	if windows.SetConsoleMode(handle, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) != nil {
		return func() {}
	}
	return func() { _ = windows.SetConsoleMode(handle, mode) }
}
