//go:build windows

package daemoncmd

import (
	"fmt"
	"hash/fnv"
)

// socketAddress is the coordinator IPC address for a test that owns dir: the daemon listens on a
// named pipe on Windows, so the address is a pipe name made unique by the directory.
func socketAddress(dir string) string {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(dir))
	return fmt.Sprintf(`\\.\pipe\paperboat-daemoncmd-test-%x`, hash.Sum64())
}
