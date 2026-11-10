//go:build windows

package windowsopenssh

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// A grandchild of the killed PowerShell (what Install-WinGetPackage spawns) ends with it.
func TestCommandRunnerEndsTheGrandchildrenOfACancelledCommand(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	script := "$p = Start-Process ping -ArgumentList '-n','120','127.0.0.1' -PassThru -WindowStyle Hidden; " +
		"Set-Content -Path '" + pidFile + "' -Value $p.Id; Start-Sleep 120"
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := (CommandRunner{}).Run(ctx, "powershell.exe", "-NoProfile", "-Command", script); err == nil {
		t.Fatal("the command was not cancelled")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the script did not start its child: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if err != nil {
			return
		}
		event, _ := windows.WaitForSingleObject(handle, 0)
		windows.Close(handle)
		if event == windows.WAIT_OBJECT_0 {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("grandchild %d outlived its cancelled parent", pid)
}
