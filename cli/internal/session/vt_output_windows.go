//go:build windows

package session

import (
	"os"

	"golang.org/x/sys/windows"
)

// PrepareConsoleOutput makes a Windows console show what pb connect writes: VT processing on and UTF-8 as the output code page. It
// returns the call that puts both back. A legacy conhost window (cmd.exe started by conhost.exe) leaves VT processing off, so the status
// bar and the remote shell's colours and cursor moves printed as literal "←[...m" text; without UTF-8 the remote shell's non-ASCII
// output printed as mojibake ("æ—¥æœ¬èªž" for 日本語) in a real Windows Terminal window (X-24, both found on the victus desktop).
// Docker's CLI enables VT the same way (moby/term EnableVirtualTerminalProcessing).
func PrepareConsoleOutput() (restore func()) {
	handle := windows.Handle(os.Stdout.Fd())
	restoreMode := enableVirtualTerminalOutput(handle)
	restoreCodePage := useUTF8OutputCodePage(handle)
	return func() { restoreCodePage(); restoreMode() }
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

const cpUTF8 = 65001 // x/sys/windows has no CP_UTF8

func useUTF8OutputCodePage(handle windows.Handle) func() {
	var mode uint32
	if windows.GetConsoleMode(handle, &mode) != nil {
		return func() {}
	}
	previous, err := windows.GetConsoleOutputCP()
	if err != nil || previous == cpUTF8 || windows.SetConsoleOutputCP(cpUTF8) != nil {
		return func() {}
	}
	return func() { _ = windows.SetConsoleOutputCP(previous) }
}
