//go:build windows

package main

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/protocol"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestExecInputPrivateConsoleBorrowedReadiness(t *testing.T) {
	if os.Getenv("PB_EXEC_INPUT_CONSOLE_HELPER") != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
		defer cancel()
		child := exec.CommandContext(ctx, executable, "-test.run=^TestExecInputPrivateConsoleBorrowedReadiness$")
		child.Env = append(os.Environ(), "PB_EXEC_INPUT_CONSOLE_HELPER=1")
		child.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
		if output, err := child.CombinedOutput(); err != nil {
			t.Fatalf("private console fixture: %v\n%s", err, output)
		}
		return
	}
	allocate, free := execInputKernel.NewProc("AllocConsole"), execInputKernel.NewProc("FreeConsole")
	if success, _, err := allocate.Call(); success == 0 {
		t.Fatal(err)
	}
	defer free.Call()
	name, err := windows.UTF16PtrFromString("CONIN$")
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(handle), "private-exec-console")
	defer file.Close()
	var original uint32
	if err := windows.GetConsoleMode(handle, &original); err != nil {
		t.Fatal(err)
	}
	write := execInputKernel.NewProc("WriteConsoleInputW")
	inject := func(events []execConsoleEvent) {
		t.Helper()
		var written uint32
		success, _, err := write.Call(uintptr(handle), uintptr(unsafe.Pointer(&events[0])), uintptr(len(events)), uintptr(unsafe.Pointer(&written)))
		if success == 0 || written != uint32(len(events)) {
			t.Fatal(err)
		}
	}
	keyEvent := func(key, character uint16) execConsoleEvent {
		event := execConsoleEvent{Kind: 1}
		data := (*execConsoleKey)(unsafe.Pointer(&event.Data[0]))
		data.Down, data.Repeat, data.VirtualKey, data.Character = 1, 1, key, character
		return event
	}
	assertCancel := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
		defer cancel()
		var value [512]byte
		if _, err := readExecInput(ctx, file, value[:]); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("empty/non-byte console read=%v", err)
		}
	}
	assertCancel()
	handles := execInputKernel.NewProc("GetProcessHandleCount")
	countHandles := func() uint32 {
		var count uint32
		if ok, _, err := handles.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&count))); ok == 0 {
			t.Fatal(err)
		}
		return count
	}
	baselineHandles := countHandles()
	// Resize and modifier events alone do not prove a byte read can complete.
	inject([]execConsoleEvent{{Kind: 4}, keyEvent(0x10, 0)})
	assertCancel()
	var before uint32
	if err := windows.GetConsoleMode(handle, &before); err != nil {
		t.Fatal(err)
	}
	// Enter beyond the old small peek window must still make cooked input ready.
	events := make([]execConsoleEvent, 401)
	for i := 0; i < 400; i++ {
		events[i] = keyEvent('X', 'x')
	}
	events[400] = keyEvent(0x0d, '\r')
	inject(events)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	var value [2048]byte
	n, err := readExecInput(ctx, file, value[:])
	cancel()
	if err != nil || !strings.Contains(string(value[:n]), strings.Repeat("x", 400)) {
		t.Fatalf("cooked line length=%d err=%v", n, err)
	}
	var after uint32
	if err := windows.GetConsoleMode(handle, &after); err != nil || after != before {
		t.Fatal("borrowed mode changed")
	}
	// Only this private fixture configures raw VT mode, as a real PTY owner does.
	raw := original&^(windows.ENABLE_LINE_INPUT|windows.ENABLE_ECHO_INPUT|windows.ENABLE_PROCESSED_INPUT) | windows.ENABLE_VIRTUAL_TERMINAL_INPUT
	if err := windows.SetConsoleMode(handle, raw); err != nil {
		t.Fatal(err)
	}
	inject([]execConsoleEvent{keyEvent(0x25, 0)})
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	n, err = readExecInput(ctx, file, value[:])
	cancel()
	if err != nil || n == 0 || value[0] != 0x1b {
		t.Fatalf("VT arrow bytes=%q err=%v", value[:n], err)
	}
	if err := windows.GetConsoleMode(handle, &after); err != nil || after != raw {
		t.Fatal("borrowed raw mode changed")
	}
	// A modifier still emits no raw bytes, and leaves cancellation bounded.
	inject([]execConsoleEvent{keyEvent(0x10, 0)})
	assertCancel()
	var queuedBefore uint32
	if err := windows.GetNumberOfConsoleInputEvents(handle, &queuedBefore); err != nil {
		t.Fatal(err)
	}
	overbound := make([]execConsoleEvent, protocol.MaxBinaryFrame/int(unsafe.Sizeof(execConsoleEvent{}))+1)
	for i := range overbound {
		overbound[i] = keyEvent(0x10, 0)
	}
	inject(overbound)
	var queued uint32
	if err := windows.GetNumberOfConsoleInputEvents(handle, &queued); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	_, err = readExecInput(ctx, file, value[:])
	cancel()
	if !errors.Is(err, errExecInputLimit) {
		t.Fatal("overbound console input did not fail before mutation")
	}
	var remaining uint32
	if err := windows.GetNumberOfConsoleInputEvents(handle, &remaining); err != nil || remaining != queued || remaining <= queuedBefore {
		t.Fatal("resource limit consumed caller console input")
	}
	if err := windows.GetConsoleMode(handle, &after); err != nil || after != raw {
		t.Fatal("resource limit changed caller mode")
	}
	if _, err := file.Stat(); err != nil {
		t.Fatal("borrowed console closed")
	}
	if count := countHandles(); count > baselineHandles {
		t.Fatalf("borrowed console reader leaked handles: before=%d after=%d", baselineHandles, count)
	}
}
