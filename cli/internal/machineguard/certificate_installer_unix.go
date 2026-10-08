//go:build linux || darwin

package machineguard

import (
	"errors"
	"os"
	"path/filepath"
)

func requireInstallerPrivilege() error { return requirePrivilege() }

func writeRenewalJournal(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".local-ca-renewal-")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err = file.Chmod(0600); err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return err
	}
	return replaceStateFile(temp, path)
}
