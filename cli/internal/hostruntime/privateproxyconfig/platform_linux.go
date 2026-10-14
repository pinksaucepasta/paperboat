//go:build linux

package privateproxyconfig

import (
	"errors"
	"os"
	"path/filepath"
)

func NewPlatformManager(stateRoot string) (*Manager, error) {
	return New(filepath.Join(stateRoot, "private-access", "system-proxy.json"), NewLinuxAdapter(ExecRunner{}, os.Getenv))
}

func validateProxyJournal(_ *os.File, info os.FileInfo) error {
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return errors.New("private proxy journal must be a regular 0600 file")
	}
	return nil
}

func syncProxyDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}
