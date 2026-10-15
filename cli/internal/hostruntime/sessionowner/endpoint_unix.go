//go:build linux || darwin

package sessionowner

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// PrepareNativeEndpoint restores the enrolled account's private IPC directory.
// Retirement removes it; the unprivileged owner cannot recreate its entry under
// the protected native parent when archived history is reopened later.
func PrepareNativeEndpoint(d Descriptor) error {
	if err := d.Validate(); err != nil {
		return err
	}
	directory := filepath.Dir(d.Endpoint)
	if err := checkCatalogRoot(filepath.Dir(directory), ""); err != nil {
		return err
	}
	created := false
	if err := os.Mkdir(directory, 0700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return catalogFailure(err)
	}
	fd, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return catalogFailure(err)
	}
	defer unix.Close(fd)
	if created {
		if err := unix.Fchown(fd, d.UID, d.GID); err != nil {
			return catalogFailure(err)
		}
		if err := unix.Fchmod(fd, 0700); err != nil {
			return catalogFailure(err)
		}
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		return catalogFailure(err)
	}
	if info.Uid != uint32(d.UID) || info.Gid != uint32(d.GID) || info.Mode&0777 != 0700 {
		return ErrInvalid
	}
	return nil
}
