//go:build windows

package pty

import (
	"fmt"

	"golang.org/x/sys/windows"
)

var setConsoleCtrlHandlerProc = windows.NewLazySystemDLL("kernel32.dll").NewProc("SetConsoleCtrlHandler")

// setIgnoreCtrlC sets (true) or clears (false) this process's "ignore Ctrl+C" flag, which child processes inherit.
// SetConsoleCtrlHandler(NULL, FALSE) restores normal Ctrl+C handling; it works from a console-less service too.
func setIgnoreCtrlC(ignore bool) error {
	add := uintptr(0)
	if ignore {
		add = 1
	}
	if r, _, err := setConsoleCtrlHandlerProc.Call(0, add); r == 0 {
		return fmt.Errorf("SetConsoleCtrlHandler: %w", err)
	}
	return nil
}
