//go:build !windows

package service

import (
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
)

func writeServiceDefinition(path string, data []byte, mode os.FileMode) error {
	return atomicfile.Write(path, data, atomicfile.Options{Mode: mode, OwnerUID: -1, OwnerGID: -1})
}

func (i *Installer) publishDefinitionBytes(data []byte) error {
	if !i.config.AccountReadableDefinition {
		return atomicWrite(i.definitionPath, data, 0600)
	}
	if os.Geteuid() != 0 {
		return ErrInvalidDefinition
	}
	group, err := user.LookupGroup(i.config.Group)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(group.Gid)
	if err != nil || gid < 0 {
		return ErrInvalidDefinition
	}
	if err := prepareAtomicDirectory(filepath.Dir(i.definitionPath)); err != nil {
		return err
	}
	return atomicfile.Write(i.definitionPath, data, atomicfile.Options{Mode: 0640, OwnerUID: 0, OwnerGID: gid})
}
