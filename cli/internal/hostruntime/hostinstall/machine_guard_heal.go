//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
)

// HealMachineGuard runs in the privileged updater on start. When the machine guard answers but runs
// another version than the installed one (an older updater drove the update, so it never replaced the
// guard), it installs the guard from the installed executable, keeping the guard's local access
// domain, and restores the replaced guard if the new one is not ready. A missing or unreachable guard
// is left to pb install and doctor. It reports whether the guard was replaced.
// Prior art: tailscaled restarts its helpers from its own package on start.
func HealMachineGuard(ctx context.Context, source installsource.Source, executable string) (bool, error) {
	if err := source.Verify(executable); err != nil {
		return false, hostInstallFailure("verify Paperboat machine guard executable", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	readyErr := existingMachineGuardReady(probeCtx, source.Version, healGuardRevision())
	domain := ""
	if readyErr != nil {
		domain = existingMachineGuardDomain(probeCtx)
	}
	cancel()
	if readyErr == nil || domain == "" {
		return false, nil
	}
	if err := prepareMachineGuardForInstall(); err != nil {
		return false, hostInstallFailure("prepare Paperboat machine guard directory", readyErr, err)
	}
	undo, err := installMissingMachineGuard(ctx, executable, domain, &machineguard.UnitSet{}) // executable may be another build
	if errors.Is(err, machineguard.ErrTrustPending) {
		err = nil
	}
	if err == nil {
		verifyCtx, cancelVerify := context.WithTimeout(ctx, 15*time.Second)
		err = existingMachineGuardReady(verifyCtx, source.Version, healGuardRevision())
		cancelVerify()
	}
	if err != nil {
		return false, errors.Join(hostInstallFailure("replace the Paperboat machine guard", readyErr, err), finishMachineGuard(ctx, undo, err))
	}
	if undo != nil {
		undo.Discard()
	}
	return true, nil
}

// healGuardRevision is the guard revision the heal judges by. Linux and macOS heal in the privileged updater
// started from the installed build (hostruntimecmd/updated_unix.go, healMachineGuard), so this process's
// revision is the installed build's, as for pb doctor. Windows heals in the SYSTEM carrier service
// (hostruntimecmd/carrier_windows.go), which an installer leaves running and so may be an older build than
// the one installed: its revision is unknown (0), and only the installed version or a release not older is kept.
func healGuardRevision() int {
	if runtime.GOOS == "windows" {
		return 0
	}
	return machineguard.Revision
}
