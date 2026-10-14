//go:build windows && paperboat_native_e2e

package pty

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The enrolled daemon receives private NUL handles, not null handles. Model
// that launch boundary only during Start, restoring the test host immediately.
func launchWithNULParent(t *testing.T, launch func() (*Process, error)) (*Process, error) {
	t.Helper()
	ids := []uint32{windows.STD_INPUT_HANDLE, windows.STD_OUTPUT_HANDLE, windows.STD_ERROR_HANDLE}
	access := []uint32{windows.GENERIC_READ, windows.GENERIC_WRITE, windows.GENERIC_WRITE}
	var saved, handles []windows.Handle
	defer func() {
		for i, handle := range saved {
			_ = windows.SetStdHandle(ids[i], handle)
		}
		for _, handle := range handles {
			_ = windows.Close(handle)
		}
	}()
	security := windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), InheritHandle: 1}
	for i, id := range ids {
		previous, err := windows.GetStdHandle(id)
		if err != nil {
			return nil, err
		}
		handle, err := windows.CreateFile(windows.StringToUTF16Ptr("NUL"), access[i], windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, &security, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			return nil, err
		}
		handles = append(handles, handle)
		saved = append(saved, previous)
		if err := windows.SetStdHandle(id, handle); err != nil {
			return nil, err
		}
	}
	return launch()
}

func TestNativeConPTYSeparatesServiceNULHandles(t *testing.T) {
	for _, name := range []string{"cmd.exe", "powershell.exe"} {
		t.Run(name, func(t *testing.T) {
			adapter, root := nativeAdapter(t)
			path := filepath.Join(os.Getenv("SystemRoot"), "System32", name)
			args := []string{"/d", "/q"}
			command := "set PB_SERVICE_SUFFIX=EXECUTED\r\necho PB_SERVICE_%PB_SERVICE_SUFFIX%\r\nexit /b 7\r\n"
			if name == "powershell.exe" {
				path = nativePowerShell(t)
				args = []string{"-NoLogo", "-NoProfile", "-NoExit"}
				command = "Write-Output ('PB_SERVICE_'+'EXECUTED'); exit 7\r\n"
			}
			process, err := launchWithNULParent(t, func() (*Process, error) {
				return adapter.Start(Command{Path: path, Args: args, CWD: root, Dimensions: Dimensions{80, 25}})
			})
			if err != nil {
				t.Fatal(err)
			}
			output := collectNativeOutput(process)
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if _, err := process.Terminate(ctx, 10*time.Millisecond); err != nil {
					t.Errorf("terminate test PTY: %v", err)
				}
				_ = process.CloseIO()
				select {
				case <-output.done:
				case <-ctx.Done():
					t.Error("test PTY output reader did not stop")
				}
			})
			if _, err := process.Write([]byte(command)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := process.Wait(ctx)
			if err != nil || result.Code != 7 {
				t.Fatalf("service-parent shell did not execute test command: result=%+v err=%v", result, err)
			}
			deadline := time.Now().Add(time.Second)
			for !strings.Contains(output.String(), "PB_SERVICE_EXECUTED") && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			if !strings.Contains(output.String(), "PB_SERVICE_EXECUTED") {
				t.Fatal("service-parent shell output did not reach ConPTY")
			}
		})
	}
}
