//go:build linux

package processlifetime

import (
	"context"

	"golang.org/x/sys/unix"
)

// ArmParentDeath asks the kernel to terminate this process when its current
// parent exits. The second parent check closes the fork-to-prctl race.
func ArmParentDeath(_ context.Context) error {
	parent := unix.Getppid()
	if parent <= 1 {
		return ErrParentUnavailable
	}
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, uintptr(unix.SIGTERM), 0, 0, 0); err != nil {
		return err
	}
	if unix.Getppid() != parent {
		return ErrParentUnavailable
	}
	return nil
}
