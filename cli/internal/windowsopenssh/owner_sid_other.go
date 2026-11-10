//go:build !windows

package windowsopenssh

import (
	"os"

	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
)

func validOwnerSID(owner string) bool { return owner != "" }

func serviceConfigFileOptions(string) atomicfile.Options {
	return atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}
}

func managedKeysFileOptions() atomicfile.Options {
	return atomicfile.Options{Mode: 0o600, OwnerUID: -1, OwnerGID: -1}
}

func setManagedKeysSecurity(string) error { return nil }

func platformOwnerSID() string { return "" }

func platformSSHComputerName() (string, error) { return os.Hostname() }
