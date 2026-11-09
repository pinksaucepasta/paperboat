//go:build windows

package hostinstall

import (
	"os"
	"path/filepath"
	"regexp"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// retiredImageName is exactly what removeWindowsFileWithRetry produces for the two rollback slots: the account's
// releases\pb.rollback.exe and a pinned version's pb.exe.rollback, each with ".retired-<base36 time>".
var retiredImageName = regexp.MustCompile(`^pb(\.rollback\.exe|\.exe\.rollback)\.retired-[0-9a-z]{1,16}$`)

// SweepWindowsRetiredImages removes the executables removeWindowsFileWithRetry moved aside because a process
// was still running them, and reports how many are still in use. After a rollback only the active version may
// remain on disk, so the updater service repeats this until none is left. It looks only where those files are
// created (the releases directory and each pinned version directory) and never through a link or junction.
func SweepWindowsRetiredImages(layout service.Layout) int {
	directories := []string{layout.ReleasesRoot}
	versions := filepath.Join(layout.ReleasesRoot, "versions")
	if entries, err := realDirectoryEntries(versions); err == nil {
		for _, entry := range entries {
			if entry.Type().IsDir() {
				directories = append(directories, filepath.Join(versions, entry.Name()))
			}
		}
	}
	remaining := 0
	for _, directory := range directories {
		entries, err := realDirectoryEntries(directory)
		if err != nil {
			continue // unreadable or not a plain directory: not ours to judge; the next sweep sees it again
		}
		for _, entry := range entries {
			if !entry.Type().IsRegular() || !retiredImageName.MatchString(entry.Name()) {
				continue
			}
			if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil && !installNotExistOnly(err) {
				remaining++
			}
		}
	}
	return remaining
}

// realDirectoryEntries lists a directory that is itself a real directory (a junction or symlink has a
// non-directory Lstat type in Go and is refused).
func realDirectoryEntries(path string) ([]os.DirEntry, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsDir() || info.Mode()&(os.ModeSymlink|os.ModeIrregular) != 0 {
		return nil, ErrInvalidRequest
	}
	return os.ReadDir(path)
}
