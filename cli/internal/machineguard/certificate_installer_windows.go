//go:build windows

package machineguard

import (
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/atomicfile"
	"github.com/pinksaucepasta/paperboat/internal/windows/elevation"
)

// Installation runs in the administrator's elevation bridge. The signer
// remains restricted to LocalSystem by requirePrivilege.
func requireInstallerPrivilege() error {
	if !elevation.IsCurrentProcessElevated() {
		return errors.New("preparing local browser trust requires an elevated administrator")
	}
	return nil
}
func writeRenewalJournal(path string, data []byte) error {
	return atomicfile.Write(path, data, atomicfile.Options{Mode: 0600, OwnerUID: -1, OwnerGID: -1, SecurityDescriptor: "O:BAD:P(A;;FA;;;SY)(A;;FA;;;BA)"})
}
