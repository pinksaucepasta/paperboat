// Package climan carries the manual with the authenticated executable.
package climan

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed man/man1/*.1
var manuals embed.FS

const manualMarker = ".\\\" Paperboat managed manual v1\n"

// Install writes the manuals from this binary to a man root (not its man1
// subdirectory). Each page is replaced atomically. A failed invocation can be
// retried; unrelated pages and directories are never removed.
func Install(directory string) (resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = manualFailure{message: "Manual installation did not finish; some pages may already be installed. Retry pb install.", cause: resultErr}
		}
	}()
	directory, err := filepath.Abs(directory)
	if err != nil {
		return err
	}
	section := filepath.Join(directory, "man1")
	if err := os.MkdirAll(section, 0755); err != nil {
		return err
	}
	if err := realDirectory(section); err != nil {
		return err
	}
	entries, err := manuals.ReadDir("man/man1")
	if err != nil {
		return err
	}
	current, wrote := make(map[string]bool, len(entries)), false
	for _, entry := range entries {
		data, err := manuals.ReadFile("man/man1/" + entry.Name())
		if err != nil {
			return err
		}
		page, path := append([]byte(manualMarker), data...), filepath.Join(section, entry.Name())
		if unchanged(path, page) {
			current[entry.Name()] = true // 268 pages with two fsyncs each outlasted the updater's deadline on a loaded 1-vCPU VM (pbvm1 F14-09)
			continue
		}
		if err := writePage(path, page); err != nil {
			return fmt.Errorf("install manual %s: %w", entry.Name(), err)
		}
		current[entry.Name()], wrote = true, true
	}
	// One directory sync makes every rename durable. The pages are regenerable, and a page the crash left empty
	// differs in size, so the next refresh rewrites it (dpkg unpacks the same way and syncs once at the end).
	if wrote {
		if err := syncDirectory(section); err != nil {
			return err
		}
	}
	return removeOwned(section, current)
}

// syncFile is the only fsync of a refresh; tests count it.
// Windows cannot flush a directory handle (FlushFileBuffers needs a file opened for writing) and NTFS journals
// the rename, so a directory is not synced there.
var syncFile = func(file *os.File) error {
	if info, err := file.Stat(); runtime.GOOS == "windows" && err == nil && info.IsDir() {
		return nil
	}
	return file.Sync()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(syncFile(directory), directory.Close())
}

// writePage replaces path with page through a temporary file in the same directory and a rename: readers see the
// old or the new page. Like atomicfile it refuses a destination that is not a regular file; unlike it, it does
// not sync (a refresh of 268 pages with two fsyncs each outlasted the updater's deadline, pbvm1 F14-09).
func writePage(path string, page []byte) (resultErr error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("destination is not a regular file")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.Remove(temporary.Name()))
		}
	}()
	if _, err := temporary.Write(page); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Chmod(0644); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// unchanged reports whether path is already a regular, world-readable file holding exactly page (Windows has no
// POSIX mode: it reports 0666, and access comes from the directory's ACL).
func unchanged(path string, page []byte) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm() != 0644) || info.Size() != int64(len(page)) {
		return false
	}
	existing, err := os.ReadFile(path)
	return err == nil && bytes.Equal(existing, page)
}

// Remove removes only pages bearing our ownership marker, preserving shared
// man directories, symlinks and third-party documentation.
func Remove(directory string) (resultErr error) {
	defer func() {
		if resultErr != nil {
			resultErr = manualFailure{message: "Owned manual removal did not finish. Retry pb uninstall after checking file permissions.", cause: resultErr}
		}
	}()
	section := filepath.Join(directory, "man1")
	if _, err := os.Lstat(section); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := realDirectory(section); err != nil {
		return err
	}
	return removeOwned(section, nil)
}

func realDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("manual destination must be a real directory")
	}
	return nil
}

func removeOwned(section string, keep map[string]bool) error {
	entries, err := os.ReadDir(section)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] || (name != "pb.1" && (!strings.HasPrefix(name, "pb-") || !strings.HasSuffix(name, ".1"))) || !entry.Type().IsRegular() {
			continue
		}
		path := filepath.Join(section, name)
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		prefix := make([]byte, len(manualMarker))
		_, readErr := io.ReadFull(file, prefix)
		closeErr := file.Close()
		if closeErr != nil || (readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF) {
			return errors.Join(readErr, closeErr)
		}
		if readErr != nil || !bytes.Equal(prefix, []byte(manualMarker)) {
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
}

type manualFailure struct {
	message string
	cause   error
}

func (e manualFailure) Error() string { return e.message }
func (e manualFailure) Unwrap() error { return e.cause }
