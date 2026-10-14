package configsync

import (
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var ErrConfigRepositoryInvalid = errors.New("invalid config repository")

func ValidateConfigRepository(root string) error {
	if !canonicalAbsolutePath(root) {
		return ErrConfigRepositoryInvalid
	}
	return filepath.WalkDir(root, func(full string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, full)
		if err != nil || relative == "." {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative == ".git" || strings.HasPrefix(relative, ".git/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !safePortableRepositoryPath(relative) {
			return ErrConfigRepositoryInvalid
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: repository symlink at %q", ErrConfigRepositoryInvalid, relative)
		}
		if !entry.IsDir() {
			info, infoErr := entry.Info()
			if infoErr != nil || !info.Mode().IsRegular() {
				return errors.Join(ErrConfigRepositoryInvalid, infoErr)
			}
		}
		return nil
	})
}

func writePrivateAtomic(path string, data []byte) error {
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.Join(ErrConfigRepositoryInvalid, err)
	}
	return atomicfile.Write(path, data, atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1})
}

func canonicalAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path
}
