package hostinstall

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// RemovePathPreservingCLI removes an owned root without following symlinks.
// The caller must authorize root and the exact CLI path before calling.
func RemovePathPreservingCLI(ctx context.Context, root, keep string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	preserve := keep != "" && cliWithin(root, keep)
	if !preserve {
		return os.RemoveAll(root)
	}
	if root == keep {
		return nil
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("CLI preservation ancestor is not a directory")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	var result error
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		result = errors.Join(result, RemovePathPreservingCLI(ctx, filepath.Join(root, entry.Name()), keep))
	}
	return result
}

// CLIOnlyRemains reports whether a root is absent or contains only the exact
// retained executable and its necessary ancestors; symlinks are never followed.
func CLIOnlyRemains(ctx context.Context, root, keep string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	info, err := os.Lstat(root)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if keep == "" || !cliWithin(root, keep) {
		return false, nil
	}
	if root == keep {
		return info.Mode().IsRegular(), nil
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		ok, err := CLIOnlyRemains(ctx, filepath.Join(root, entry.Name()), keep)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}
func cliWithin(root, keep string) bool {
	rel, err := filepath.Rel(root, keep)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
