//go:build windows

package configsync

import (
	"io/fs"

	"github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"
)

func restoreRegularFile(target string, content []byte, _ fs.FileMode) error {
	return atomicfile.Write(target, content, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}
