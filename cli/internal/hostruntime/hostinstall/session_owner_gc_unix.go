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
	"time"

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
	active := coordinator.Active
	return pruneSessionOwnerPinsAt(ctx, coordinator.Catalog, paths, active.UID, active.RuntimeRoot, active.Source.SHA256, pinStagingWindow)
}

// pruneSessionOwnerPinsAt removes every pin of paths that neither the active source, a catalogue descriptor nor a
// carrier names. A pin younger than staging is kept: an install may not have published its descriptor yet.
func pruneSessionOwnerPinsAt(ctx context.Context, catalog sessionowner.Catalog, paths installPaths, uid int, runtimeRoot, active string, staging time.Duration) error {
	descriptors, err := catalog.ReadAll(ctx, paths.instance)
	if err != nil {
		return err
	}
	root := filepath.Join(filepath.Dir(paths.workerRollback), "pins")
	keep := map[string]bool{filepath.Join(root, active, "pb"): true}
	for _, d := range descriptors {
		if d.UID != uid || d.RuntimeRoot != runtimeRoot || d.Executable != filepath.Join(root, d.Source.SHA256, "pb") {
			return ErrInvalidRequest
		}
		keep[d.Executable] = true
	}
	// A retired carrier generation still serves from its pin until it exits.
	carriers, err := filepath.Glob(carrierGenerationPath(paths.instance, "*"))
	if err != nil {
		return err
	}
	for _, path := range append(carriers, carrierConfigPath(paths.instance)) {
		carrier, err := readCarrierFile(ctx, paths.instance, path)
		if installNotExistOnly(err) {
			continue
		}
		if err != nil {
			return err
		}
		if carrier.UID != uid || carrier.Executable != filepath.Join(root, carrier.Source.SHA256, "pb") {
			return ErrInvalidRequest
		}
		keep[carrier.Executable] = true
	}
	return pruneUnixSessionOwnerPins(ctx, root, keep, pinSweepBound(catalog), staging)
}

// pinStagingWindow matches the catalogue's staging window: a pin this new may
// belong to an install that has not published its descriptor yet.
const pinStagingWindow = 10 * time.Minute

// pinSweepBound is how many pin directories one sweep reads: every
// session-owner reference, the active and one retiring carrier, and one
// unreferenced candidate, so repeated sweeps make bounded progress.
func pinSweepBound(c sessionowner.Catalog) int { return c.Maximum + 3 }

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

func pruneUnixSessionOwnerPins(ctx context.Context, root string, keep map[string]bool, maximum int, staging time.Duration) (resultErr error) {
	if ctx == nil || maximum < 2 || maximum > pinSweepBound(sessionowner.Catalog{Maximum: sessionowner.MaxCatalogEntries}) {
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
		// An update stages its candidate's pin before publishing the
		// descriptor that references it; a sweep in between must not delete
		// it (pbvm4 run20: install failed opening its own candidate pin).
		if info, err := entry.Info(); err != nil || time.Since(info.ModTime()) < staging {
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
