//go:build darwin || linux

package hostinstall

import (
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
)

// SuppliedBinary uses the same fixed paths, binary journal and enrolled service
// transaction as enrollment. An unbound installation starts only the user daemon;
// account-dependent roles are installed when enrollment supplies their identity.
func SuppliedBinary(ctx context.Context, input Request, operation string) error {
	if os.Geteuid() != 0 {
		return ErrNotPrivileged
	}
	if input.Schema != SchemaV1 || input.Platform != runtime.GOOS || input.UID != invokingUID() || !validRunIdentity(input) || input.Source.Platform != runtime.GOOS || input.Source.Architecture != runtime.GOARCH {
		return ErrInvalidRequest
	}
	account, err := user.LookupId(strconv.Itoa(input.UID))
	if err != nil || account.Username != input.User || account.HomeDir != input.Home || account.Gid != strconv.Itoa(input.GID) {
		return ErrInvalidRequest
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil || group.Name != input.Group {
		return ErrInvalidRequest
	}
	if err = verifyArtifact(input.Executable, input.UID, input.Platform); err != nil {
		return err
	}
	if err = input.Source.Verify(input.Executable); err != nil {
		return err
	}
	paths := platformPaths(input.UID)
	if err := ensureSharedUserParents(paths); err != nil {
		return err
	}
	if err := secureRootDirectory(filepath.Dir(paths.metadata), 0755); err != nil {
		return err
	}
	lock, err := LockSuppliedOperation(paths.metadata+".lock", 0)
	if err != nil {
		return err
	}
	defer lock.Close()
	journal, journalErr := loadJournal(paths.journal)
	if journalErr != nil && !errors.Is(journalErr, os.ErrNotExist) {
		return journalErr
	}
	if journalErr == nil && journal.Source != input.Source {
		return errors.New("another Paperboat installation is pending; finish or recover that installation before replacing its source")
	}
	previous, err := loadInstallMetadata(paths.metadata, input.UID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	bound := err == nil && previous.SetupMode != "awaiting_enrollment"
	request := Request{Schema: input.Schema, Platform: input.Platform, User: input.User, UID: input.UID, GID: input.GID, Group: input.Group, Home: input.Home, Executable: input.Executable, Source: input.Source, SetupMode: "awaiting_enrollment"}
	if bound {
		request = previous
		request.Executable, request.Source = input.Executable, input.Source
	} else {
		request.SetupMode = "awaiting_enrollment"
	}
	switch operation {
	case "install-supplied":
		if bound {
			return Install(ctx, request)
		}
		if err = ensureSharedUserParents(paths); err != nil {
			return err
		}
		if _, err = recoverInterrupted(paths); err != nil {
			return err
		}
		if err = secureRootDirectory(filepath.Dir(paths.worker), 0o755); err != nil {
			return err
		}
		if err = stageBinary(request.Executable, paths.workerNext, request.Source); err != nil {
			return err
		}
		journal, err := newInstallJournal(paths, request.Source)
		if err != nil {
			return err
		}
		if err = writeJournal(paths.journal, journal); err != nil {
			return err
		}
		if err = activateBinary(paths.worker, paths.workerNext, paths.workerRollback); err != nil {
			return errors.Join(err, rollbackFiles(paths, journal))
		}
		journal.Stage = "services_started"
		return writeJournal(paths.journal, journal)
	case "commit-supplied":
		request.Executable = paths.worker
		if err = request.Source.Verify(paths.worker); err != nil {
			return err
		}
		if bound {
			return Commit(request)
		}
		return commitPreparedInstallation(request, paths)
	case "rollback-supplied":
		journal, err := loadJournal(paths.journal)
		if err != nil {
			return err
		}
		if err = rollbackFiles(paths, journal); err != nil {
			return err
		}
		if bound {
			previous.Executable = paths.worker
			return Repair(ctx, previous)
		}
		return nil
	default:
		return ErrInvalidRequest
	}
}

// LockSuppliedOperation serializes a caller's complete transaction, or a root
// helper's mutation, without leaving a stale lock after the process exits.
func LockSuppliedOperation(path string, uid int) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || ownerUID(info) != uid || info.Mode().Perm() != 0600 {
		f.Close()
		return nil, ErrInvalidRequest
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another Paperboat installation is running; retry when it finishes")
	}
	return f, nil
}
