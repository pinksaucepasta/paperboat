//go:build darwin

package privateproxyconfig

import (
	"errors"
	"os"
)

// There is no NewPlatformManager on macOS: the system auto-proxy needs administrator rights and is one setting
// for every user, so the root machine guard owns it (machineguard preview_pac_darwin.go) and hostd never runs
// networksetup.

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
