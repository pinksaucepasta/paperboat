//go:build !windows

package service

import (
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"os"
)

func writeServiceDefinition(path string, data []byte, mode os.FileMode) error {
	return atomicfile.Write(path, data, atomicfile.Options{Mode: mode, OwnerUID: -1, OwnerGID: -1})
}
