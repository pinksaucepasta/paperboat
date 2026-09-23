//go:build darwin || linux

package updated

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updateflow"
)

type nativeInstallSnapshot struct {
	Journal []byte `json:"journal"`
}

func nativeInstallPath(root string) string { return filepath.Join(root, "native-install.json") }
func nativeInstallPending(root string) (bool, error) {
	_, err := os.Lstat(nativeInstallPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// LockUnixNativeInstall excludes update activation before native crash recovery.
func LockUnixNativeInstall(root string) (io.Closer, error) {
	lock, err := unixActivationLock(root)
	if err != nil {
		return nil, fmt.Errorf("finish the pending update before installing: %w", err)
	}
	return lock, nil
}

// PrepareUnixNativeInstall requires the caller to hold the activation lock.
func PrepareUnixNativeInstall(root string) error {
	if pending, err := nativeInstallPending(root); err != nil || pending {
		return errors.Join(ErrActivationPending, err)
	}
	if handoff, err := readUnixHandoff(root); err != nil || handoff != nil {
		return fmt.Errorf("finish or recover the pending update before installing: %w", errors.Join(ErrActivationPending, err))
	}
	path := filepath.Join(root, "transaction.json")
	journal, err := updateflow.Load(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && journal.Stage != updateflow.StageIdle {
		return fmt.Errorf("finish or recover the pending update before installing: %w", ErrActivationPending)
	}
	var snapshot nativeInstallSnapshot
	if err == nil {
		snapshot.Journal, err = os.ReadFile(path)
		if err != nil {
			return err
		}
	}
	body, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err = atomicfile.Write(nativeInstallPath(root), body, atomicfile.Options{Mode: 0600, OwnerUID: 0, OwnerGID: 0}); err != nil {
		return err
	}
	if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(err, RollbackUnixNativeInstall(root))
	}
	if err = syncUnixDirectory(root); err != nil {
		return errors.Join(err, RollbackUnixNativeInstall(root))
	}
	return nil
}

// CommitUnixNativeInstall runs under the native installer's operation lock.
func CommitUnixNativeInstall(root string) error {
	body, readErr := os.ReadFile(nativeInstallPath(root))
	if errors.Is(readErr, os.ErrNotExist) {
		return nil
	}
	if readErr != nil {
		return readErr
	}
	if err := os.Remove(nativeInstallPath(root)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncUnixDirectory(root); err != nil {
		if readErr == nil {
			return errors.Join(err, atomicfile.Write(nativeInstallPath(root), body, atomicfile.Options{Mode: 0600, OwnerUID: 0, OwnerGID: 0}))
		}
		return err
	}
	return nil
}

// RollbackUnixNativeInstall restores the previous idle transaction before lifting
// the pending marker. Updater startup and activation honor that marker.
func RollbackUnixNativeInstall(root string) error {
	info, err := os.Lstat(nativeInstallPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := secureRoot(root); err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || owner.Uid != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > 256<<10 {
		return ErrInvalidConfig
	}
	body, err := os.ReadFile(nativeInstallPath(root))
	if err != nil {
		return err
	}
	var snapshot nativeInstallSnapshot
	if len(body) > 256<<10 || json.Unmarshal(body, &snapshot) != nil {
		return ErrInvalidConfig
	}
	path := filepath.Join(root, "transaction.json")
	if len(snapshot.Journal) > 0 {
		var previous updateflow.Journal
		if json.Unmarshal(snapshot.Journal, &previous) != nil || previous.Validate() != nil || previous.Stage != updateflow.StageIdle {
			return ErrInvalidConfig
		}
		if err = atomicfile.Write(path, snapshot.Journal, atomicfile.Options{Mode: 0600, OwnerUID: 0, OwnerGID: 0}); err != nil {
			return err
		}
	} else if err = os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return CommitUnixNativeInstall(root)
}
