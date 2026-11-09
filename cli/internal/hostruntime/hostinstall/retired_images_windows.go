//go:build windows

package hostinstall

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// SweepWindowsRetiredImages removes the executables removeWindowsFileWithRetry moved aside because a process
// was still running them, and reports how many are still in use. After a rollback only the active version may
// remain on disk, so the updater service repeats this until none is left.
func SweepWindowsRetiredImages(layout service.Layout) int {
	remaining := 0
	_ = filepath.WalkDir(layout.ReleasesRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.Contains(entry.Name(), windowsRetiredSuffix) {
			return nil // unreadable entries are not ours to judge; the next sweep sees them again
		}
		if removeErr := os.Remove(path); removeErr != nil && !installNotExistOnly(removeErr) {
			remaining++
		}
		return nil
	})
	return remaining
}
