//go:build load && windows

package execprocess

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// execLoadFDs is the process's open handle count (the Windows counterpart of the descriptor count).
func execLoadFDs() int {
	var count uint32
	handle, _ := windows.GetCurrentProcess()
	proc := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetProcessHandleCount")
	if ok, _, _ := proc.Call(uintptr(handle), uintptr(unsafe.Pointer(&count))); ok == 0 {
		return 0
	}
	return int(count)
}

func loadBaseEnvironment() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "PATHEXT=" + os.Getenv("PATHEXT"), "SystemRoot=" + os.Getenv("SystemRoot"),
		"TEMP=" + os.Getenv("TEMP"), "TMP=" + os.Getenv("TMP"), "WINDIR=" + os.Getenv("WINDIR")}
}

func loadCmd() string { return filepath.Join(os.Getenv("WINDIR"), "System32", "cmd.exe") }

// loadBusyArgv prints the given number of bytes (a file in root, so no PowerShell is started 32 times at once), then
// stays alive for about two seconds (three pings). The path is unquoted: Go would escape quotes in a way cmd does not parse.
func loadBusyArgv(root string, bytes int) []string {
	file := filepath.Join(root, "busy-output.txt")
	if info, err := os.Stat(file); err != nil || info.Size() != int64(bytes) { // written once: earlier executions are reading it
		if err := os.WriteFile(file, []byte(strings.Repeat("x", bytes)), 0o600); err != nil {
			panic(err)
		}
	}
	return []string{loadCmd(), "/c", fmt.Sprintf(`type %s & ping -n 3 127.0.0.1 >nul`, file)}
}

func loadTrueArgv() []string { return []string{loadCmd(), "/c", "exit 0"} }
func loadEchoArgv() []string { return []string{loadCmd(), "/c", "echo ok"} }

// loadFDBaseline is the handle count a leak is measured from: the count after the 32 concurrent executions, because
// every OS thread the Go runtime starts for them holds handles until the process ends (603 against 188 at the start,
// and 205 after 300 sequential executions: no per-execution leak).
func loadFDBaseline(_, afterConcurrent int) int { return afterConcurrent }
