//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
		"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
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
	readyErr := existingMachineGuardReady(probeCtx, source.Version)
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
	undo, err := installMissingMachineGuard(ctx, executable, domain)
	if errors.Is(err, machineguard.ErrTrustPending) {
		err = nil
	}
	if err == nil {
		verifyCtx, cancelVerify := context.WithTimeout(ctx, 15*time.Second)
		err = existingMachineGuardReady(verifyCtx, source.Version)
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
