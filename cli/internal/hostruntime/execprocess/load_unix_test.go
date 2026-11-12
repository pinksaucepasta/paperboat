//go:build load && (darwin || linux)

package execprocess

import (
	"fmt"
	"os"
)

func execLoadFDs() int {
	entries, _ := os.ReadDir("/proc/self/fd")
	return len(entries)
}

func loadBaseEnvironment() []string { return []string{"PATH=/usr/bin:/bin", "LANG=C"} }

// loadBusyArgv prints the given number of bytes, then stays alive for two seconds.
func loadBusyArgv(_ string, bytes int) []string {
	return []string{"/bin/sh", "-c", fmt.Sprintf("head -c %d /dev/zero | tr '\\0' x; sleep 2", bytes)}
}

func loadTrueArgv() []string { return []string{"/bin/true"} }
func loadEchoArgv() []string { return []string{"/bin/echo", "ok"} }

// loadFDBaseline is the descriptor count a leak is measured from: the count at the start of the test.
func loadFDBaseline(start, _ int) int { return start }
