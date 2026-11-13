//go:build windows

package session

import (
	"testing"

	"golang.org/x/sys/windows"
)

// x/sys/windows has no CreateConsoleScreenBuffer.
func createConsoleScreenBuffer() (windows.Handle, error) {
	handle, _, err := windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateConsoleScreenBuffer").Call(
		uintptr(windows.GENERIC_READ|windows.GENERIC_WRITE), uintptr(windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE), 0, 1, 0) // 1 = CONSOLE_TEXTMODE_BUFFER
	if windows.Handle(handle) == windows.InvalidHandle {
		return 0, err
	}
	return windows.Handle(handle), nil
}

// A console screen buffer with VT processing off (what a legacy conhost window hands pb) gets it on, and the old mode comes back.
func TestEnableVirtualTerminalOutputTurnsOnAndRestores(t *testing.T) {
	buffer, err := createConsoleScreenBuffer()
	if err != nil {
		t.Skipf("no console attached to this test process: %v", err)
	}
	defer windows.CloseHandle(buffer)
	var before uint32
	if err := windows.GetConsoleMode(buffer, &before); err != nil {
		t.Fatal(err)
	}
	if err := windows.SetConsoleMode(buffer, before&^windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING); err != nil {
		t.Fatal(err)
	}
	restore := enableVirtualTerminalOutput(buffer)
	var during uint32
	_ = windows.GetConsoleMode(buffer, &during)
	if during&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING == 0 {
		t.Fatalf("VT processing not enabled: mode %#x", during)
	}
	restore()
	var after uint32
	_ = windows.GetConsoleMode(buffer, &after)
	if after&windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING != 0 {
		t.Fatalf("old mode not restored: mode %#x", after)
	}
}

// The output code page becomes UTF-8 for a console handle and is put back afterwards.
func TestUseUTF8OutputCodePageSwitchesAndRestores(t *testing.T) {
	buffer, err := createConsoleScreenBuffer()
	if err != nil {
		t.Skipf("no console attached to this test process: %v", err)
	}
	defer windows.CloseHandle(buffer)
	previous, err := windows.GetConsoleOutputCP()
	if err != nil {
		t.Skipf("no console code page: %v", err)
	}
	defer func() { _ = windows.SetConsoleOutputCP(previous) }()
	if err := windows.SetConsoleOutputCP(437); err != nil {
		t.Skipf("cannot set a code page: %v", err)
	}
	restore := useUTF8OutputCodePage(buffer)
	if cp, _ := windows.GetConsoleOutputCP(); cp != cpUTF8 {
		t.Fatalf("output code page %d, want 65001", cp)
	}
	restore()
	if cp, _ := windows.GetConsoleOutputCP(); cp != 437 {
		t.Fatalf("code page not restored: %d", cp)
	}
}
