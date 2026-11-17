//go:build linux || darwin

package machineguard

import "github.com/pinksaucepasta/paperboat/cli/internal/atomicfile"

func requireInstallerPrivilege() error { return requirePrivilege() }

func writeRenewalJournal(path string, data []byte) error {
	return atomicfile.Write(path, data, atomicfile.CurrentOwnerOptions(0600))
}
