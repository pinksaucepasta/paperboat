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
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

//go:embed man/man1/*.1
var manuals embed.FS

const manualMarker = ".\\\" Paperboat managed manual v1\n"

// Install writes the manuals from this binary to a man root (not its man1
// subdirectory). Each page is replaced atomically. A failed invocation can be
// retried; unrelated pages and directories are never removed.
func Install(directory string) error {
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
	current := make(map[string]bool, len(entries))
	for _, entry := range entries {
		data, err := manuals.ReadFile("man/man1/" + entry.Name())
		if err != nil {
			return err
		}
		if err := atomicfile.Write(filepath.Join(section, entry.Name()), append([]byte(manualMarker), data...), atomicfile.Options{Mode: 0644, OwnerUID: -1, OwnerGID: -1}); err != nil {
			return fmt.Errorf("install manual %s: %w", entry.Name(), err)
		}
		current[entry.Name()] = true
	}
	return removeOwned(section, current)
}

// Remove removes only pages bearing our ownership marker, preserving shared
// man directories, symlinks and third-party documentation.
func Remove(directory string) error {
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
		if closeErr != nil {
			return closeErr
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
