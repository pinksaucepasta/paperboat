//go:build !windows

package splitdns

import (
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"os"
)

func writeCAState(path string, data []byte, mode os.FileMode) error {
	return atomicfile.Write(path, data, atomicfile.Options{Mode: mode, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid()})
}
