//go:build !windows

package config

import (
	"errors"
	"os"
)

func prepareSharedLockParent(path string) error {
	return os.MkdirAll(path, 0o700)
}

func createSharedLockDirectory(path string) error {
	return os.Mkdir(path, 0o700)
}

func validateSharedLockDirectory(string) error { return nil }

// writeSharedLockOwner publishes owner.json whole or not at all: a full disk or a crash leaves no empty owner.
func writeSharedLockOwner(path string, data []byte) (resultErr error) {
	tmp, err := os.OpenFile(path+".tmp", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, os.Remove(path+".tmp"))
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	//paperboat:allow-source-policy atomic-replacement owner=runtime-auth reason=atomic-owner-publish
	return os.Rename(path+".tmp", path)
}

func quarantineSharedLock(path, stalePath string) error {
	//paperboat:allow-source-policy atomic-replacement owner=runtime-auth reason=stale-lock-quarantine
	if err := os.Rename(path, stalePath); err != nil {
		return err
	}
	return os.RemoveAll(stalePath)
}

func cleanupNewSharedLock(path string) error {
	return os.Remove(path)
}
