//go:build linux || darwin

package hostinstall

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"golang.org/x/sys/unix"
)

// PendingInstalledSource resolves the fixed native account slot without taking
// installer locks: first-enrollment updater readiness precedes metadata commit.
// The boolean reports committed installation with no pending native receipt.
// Pending source is readiness identity only and cannot enable release operations.
func PendingInstalledSource(uid int, binary string) (installsource.Source, bool, error) {
	if os.Geteuid() != 0 {
		return installsource.Source{}, false, ErrNotPrivileged
	}
	if uid < 0 {
		return installsource.Source{}, false, ErrInvalidRequest
	}
	paths := platformPaths(uid)
	if binary != paths.worker {
		return installsource.Source{}, false, ErrInvalidRequest
	}
	return pendingInstalledSource(paths, uid)
}

func pendingInstalledSource(paths installPaths, uid int) (installsource.Source, bool, error) {
	// Both receipt and payload namespaces must have privileged custody. Reuse
	// the native pinned-payload ancestor validator; no directory repair occurs.
	for _, parent := range []string{filepath.Dir(paths.metadata), filepath.Dir(paths.worker)} {
		fd, err := openProtectedPinDirectory(parent)
		if err != nil {
			return installsource.Source{}, false, err
		}
		if err = unix.Close(fd); err != nil {
			return installsource.Source{}, false, err
		}
	}
	info, err := os.Lstat(paths.worker)
	if err != nil {
		return installsource.Source{}, false, err
	}
	if !info.Mode().IsRegular() || ownerUID(info) != 0 || info.Mode().Perm()&0022 != 0 {
		return installsource.Source{}, false, ErrInvalidRequest
	}

	installed, metadataErr := loadInstallMetadata(paths.metadata, uid)
	if metadataErr != nil && !installNotExistOnly(metadataErr) {
		return installsource.Source{}, false, metadataErr
	}
	journal, journalErr := loadJournal(paths.journal)
	if journalErr != nil && !installNotExistOnly(journalErr) {
		return installsource.Source{}, false, journalErr
	}
	if journalErr != nil {
		if metadataErr != nil {
			return installsource.Source{}, false, metadataErr
		}
		pending, err := pendingSourceLifecycle(paths, installed)
		if err != nil {
			return installsource.Source{}, false, err
		}
		if verifyErr := installed.Source.Verify(paths.worker); verifyErr != nil {
			// A signed helper may own an approved fixed-slot candidate before source
			// commit. Readiness observes pending; it must not bless unknown slot bytes.
			if pending {
				return installsource.Source{}, false, nil
			}
			return installsource.Source{}, false, verifyErr
		}
		return installed.Source, !installed.EnrollmentPending && !pending, nil
	}
	if !journal.MetadataCaptured || (journal.Stage != "binaries_activated" && journal.Stage != "services_started") {
		return installsource.Source{}, false, ErrInvalidRequest
	}
	if metadataErr == nil {
		current, err := readProtectedInstallState(paths.metadata, 128<<10)
		if err != nil {
			return installsource.Source{}, false, err
		}
		if !bytes.Equal(current, journal.PreviousMetadata) {
			// Metadata publication precedes receipt cleanup. Only that exact source
			// commit and the same native account identity may explain changed bytes.
			if installed.Source != journal.Source || journal.Stage != "services_started" {
				return installsource.Source{}, false, ErrInvalidRequest
			}
			var previous Request
			if err := decodeInstallJSON(bytes.NewReader(journal.PreviousMetadata), &previous, 128<<10); err != nil {
				return installsource.Source{}, false, err
			}
			if previous.Schema != SchemaV1 || previous.Platform != installed.Platform || previous.UID != uid || previous.GID != installed.GID || previous.User != installed.User || previous.Group != installed.Group || previous.Home != installed.Home {
				return installsource.Source{}, false, ErrInvalidRequest
			}
			if !previous.EnrollmentPending {
				previous.Executable, previous.Source, previous.Artifact, previous.SessionOwnerGeneration = installed.Executable, installed.Source, installed.Artifact, installed.SessionOwnerGeneration
				if previous != installed {
					return installsource.Source{}, false, ErrInvalidRequest
				}
			}
		}
	} else if len(journal.PreviousMetadata) != 0 {
		return installsource.Source{}, false, ErrInvalidRequest
	}
	if journal.Source.Platform != runtime.GOOS || journal.Source.Architecture != runtime.GOARCH {
		return installsource.Source{}, false, ErrInvalidRequest
	}
	if err := journal.Source.Verify(paths.worker); err != nil {
		return installsource.Source{}, false, err
	}
	return journal.Source, false, nil
}

func pendingSourceLifecycle(paths installPaths, request Request) (bool, error) {
	body, err := pendingSourceBaseline(context.Background(), paths, request)
	return len(body) != 0, err
}

func pendingSourceBaseline(ctx context.Context, paths installPaths, request Request) ([]byte, error) {
	stateRoot := filepath.Join(paths.root, "service-lifecycle")
	if _, err := os.Lstat(stateRoot); installNotExistOnly(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	fd, err := openProtectedPinDirectory(stateRoot)
	if err != nil {
		return nil, err
	}
	if err = unix.Close(fd); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(stateRoot, "service-lifecycle.json")); installNotExistOnly(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	hostd, updater, err := pendingInstallers(request, paths)
	if err != nil {
		return nil, err
	}
	var host *service.Installer
	if runtime.GOOS != "darwin" {
		host, err = hostInstallerWithMissing(request, paths, true)
		if err != nil {
			return nil, err
		}
	}
	lifecycle, err := newLifecycleManager(request, paths, host, hostd, updater)
	if err != nil {
		return nil, err
	}
	body, err := lifecycle.CapturedInstallation(ctx)
	if installNotExistOnly(err) {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Join(service.ErrLifecycleUncertain, err)
	}
	return body, nil
}
