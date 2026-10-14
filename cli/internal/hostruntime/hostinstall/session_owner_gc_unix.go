//go:build linux || darwin

package hostinstall

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"golang.org/x/sys/unix"
)

// PruneSessionOwnerPins runs after reconciliation under the native control
// lock. All catalogue references, including retained history, keep their pin.
// Unix owner pins have a separate namespace from native rollback/update slots.
func PruneSessionOwnerPins(ctx context.Context, coordinator sessionowner.Coordinator) error {
	if ctx == nil || coordinator.Active.Validate() != nil || os.Geteuid() != 0 {
		return ErrInvalidRequest
	}
	paths := platformPaths(coordinator.Active.UID)
	if coordinator.Catalog.Root != filepath.Join(paths.root, "session-owners") || coordinator.Active.Instance != paths.instance {
		return ErrInvalidRequest
	}
	descriptors, err := coordinator.Catalog.ReadAll(ctx, coordinator.Active.Instance)
	if err != nil {
		return err
	}
	root := filepath.Join(filepath.Dir(paths.workerRollback), "pins")
	keep := map[string]bool{filepath.Join(root, coordinator.Active.Source.SHA256, "pb"): true}
	for _, d := range descriptors {
		if d.UID != coordinator.Active.UID || d.RuntimeRoot != coordinator.Active.RuntimeRoot || d.Executable != filepath.Join(root, d.Source.SHA256, "pb") {
			return ErrInvalidRequest
		}
		keep[d.Executable] = true
	}
	carrier, err := readCarrierConfigFile(ctx, coordinator.Active.Instance)
	if err == nil {
		if carrier.UID != coordinator.Active.UID || carrier.Executable != filepath.Join(root, carrier.Source.SHA256, "pb") {
			return ErrInvalidRequest
		}
		keep[carrier.Executable] = true
	} else if !installNotExistOnly(err) {
		return err
	}
	return pruneUnixSessionOwnerPins(ctx, root, keep, coordinator.Catalog.Maximum+2)
}

// Open each ancestor without following links and validate privileged custody.
// All subsequent deletion uses directory descriptors, never recursive paths.
func openProtectedPinDirectory(path string) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, ErrInvalidRequest
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	for _, name := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if name == "" {
			continue
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return -1, err
		}
		fd = next
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			unix.Close(fd)
			return -1, err
		}
		if stat.Uid != 0 || stat.Mode&0022 != 0 {
			unix.Close(fd)
			return -1, ErrInvalidRequest
		}
	}
	return fd, nil
}

func pruneUnixSessionOwnerPins(ctx context.Context, root string, keep map[string]bool, maximum int) (resultErr error) {
	if ctx == nil || maximum < 2 || maximum > 258 {
		return ErrInvalidRequest
	}
	fd, err := openProtectedPinDirectory(root)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	directory := os.NewFile(uintptr(fd), root)
	defer func() { resultErr = errors.Join(resultErr, directory.Close()) }()
	// The bound includes all session-owner references, one stable carrier, and
	// one unreferenced candidate, so repeated sweeps make bounded progress.
	entries, err := directory.ReadDir(maximum)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if len(name) != 64 || strings.ToLower(name) != name || keep[filepath.Join(root, name, "pb")] {
			continue
		}
		if _, err := hex.DecodeString(name); err != nil {
			continue
		}
		child, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			continue
		} // Unknown files and links are never GC-owned.
		removed, err := pruneUnixSessionOwnerPin(ctx, child, name)
		closeErr := unix.Close(child)
		if err != nil {
			return errors.Join(err, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		if !removed {
			continue
		}
		// Remove only an empty directory. Unknown neighbouring files stay intact.
		if err := unix.Unlinkat(fd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOTEMPTY) && !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	return nil
}

func pruneUnixSessionOwnerPin(ctx context.Context, directory int, digest string) (removed bool, resultErr error) {
	var stat unix.Stat_t
	if err := unix.Fstat(directory, &stat); err != nil {
		return false, err
	}
	if stat.Uid != 0 || stat.Mode&0022 != 0 {
		return false, nil
	}
	fd, err := unix.Openat(directory, "pb", unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) || errors.Is(err, unix.ELOOP) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	file := os.NewFile(uintptr(fd), "pb")
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	if err := unix.Fstat(fd, &stat); err != nil {
		return false, err
	}
	if stat.Uid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0022 != 0 || stat.Mode&0111 == 0 || stat.Size <= 0 || stat.Size > installsource.MaxBytes {
		return false, nil
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, installsource.MaxBytes+1)); err != nil {
		return false, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := unix.Unlinkat(directory, "pb", 0); err != nil {
		return false, err
	}
	return true, nil
}
