//go:build windows

package bugreport

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestRecordingInputPrivateConsoleCancellationAndEnter(t *testing.T) {
	if os.Getenv("PB_RECORDING_CONSOLE_HELPER") != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, executable, "-test.run=^TestRecordingInputPrivateConsoleCancellationAndEnter$")
		command.Env = append(os.Environ(), "PB_RECORDING_CONSOLE_HELPER=1")
		// The helper receives no user console. All console changes below belong to
		// this isolated process, not to the shell running the native test executable.
		command.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("private console fixture: %v\n%s", err, output)
		}
		return
	}
	allocate := recordingKernel.NewProc("AllocConsole")
	free := recordingKernel.NewProc("FreeConsole")
	if success, _, err := allocate.Call(); success == 0 {
		t.Fatalf("create private console: %v", err)
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
	file := os.NewFile(uintptr(handle), "private-test-console")
	defer file.Close()
	var before uint32
	if err := windows.GetConsoleMode(handle, &before); err != nil {
		t.Fatal(err)
	}
	// Warm the timer path before counting handles owned by the native helper.
	canceled, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	err = waitForLine(canceled, file)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("empty console cancellation lost")
	}
	handles := recordingKernel.NewProc("GetProcessHandleCount")
	countHandles := func() uint32 {
		var count uint32
		if success, _, err := handles.Call(uintptr(windows.CurrentProcess()), uintptr(unsafe.Pointer(&count))); success == 0 {
			t.Fatalf("count private handles: %v", err)
		}
		return count
	}
	baseline := countHandles()
	for range 3 {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		err := waitForLine(ctx, file)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("console cancellation lost")
		}
	}
	event := recordingConsoleEvent{Kind: 1}
	key := (*recordingKeyEvent)(unsafe.Pointer(&event.Data[0]))
	key.Down, key.Repeat, key.VirtualKey, key.Character = 1, 1, 0x0d, '\r'
	var written uint32
	write := recordingKernel.NewProc("WriteConsoleInputW")
	if success, _, err := write.Call(uintptr(handle), uintptr(unsafe.Pointer(&event)), 1, uintptr(unsafe.Pointer(&written))); success == 0 || written != 1 {
		t.Fatalf("write private Enter event: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := waitForLine(ctx, file); err != nil {
		t.Fatal(err)
	}
	var after uint32
	if err := windows.GetConsoleMode(handle, &after); err != nil || after != before {
		t.Fatal("borrowed console mode changed")
	}
	if _, err := file.Stat(); err != nil {
		t.Fatal("borrowed console handle closed")
	}
	if count := countHandles(); count > baseline {
		t.Fatalf("private recording helper leaked handles: before=%d after=%d", baseline, count)
	}
}
