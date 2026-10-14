//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
)

// Per-user installation reuses an authenticated ready machine-wide guard.
// Explicit guard installation remains responsible for upgrading its executable.
var existingMachineGuardReady = readyMachineGuard
var prepareMachineGuardForInstall = prepareMachineGuardFoundation
var installMissingMachineGuard = machineguard.Install

func readyMachineGuard(ctx context.Context) (resultErr error) {
	client, err := machineguard.Connect(ctx, machineguard.DefaultSocket)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = withCleanup("machine guard connection cleanup failed", resultErr, client.Close())
	}()
	status, err := client.Status(ctx)
	if err != nil {
		return err
	}
	return validateMachineGuardStatus(status)
}

func validateMachineGuardStatus(status machineguard.RuntimeStatus) error {
	if !status.Ready {
		return errors.New("machine guard is not ready; retry machine-guard installation")
	}
	if status.Version != buildinfo.Version {
		return fmt.Errorf("machine guard requires upgrade to %s", buildinfo.Version)
	}
	if status.RootRenewalRequired {
		return errors.New("machine guard HTTPS root requires renewal; retry machine-guard installation")
	}
	// TrustReady may be false while browser trust awaits approval; the guard is usable.
	return nil
}
func installMachineGuard(ctx context.Context, request Request) error {
	if ctx == nil {
		return ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := request.Source.Verify(request.Executable); err != nil {
		return hostInstallFailure("verify Paperboat machine guard executable", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	readyErr := existingMachineGuardReady(probeCtx)
	cancel()
	if readyErr == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return hostInstallFailure("machine guard installation canceled", readyErr, err, context.Cause(ctx))
	}
	if err := prepareMachineGuardForInstall(); err != nil {
		return hostInstallFailure("prepare Paperboat machine guard directory", readyErr, err)
	}
	// A ready guard whose browser trust awaits approval is installed successfully.
	if err := installMissingMachineGuard(ctx, request.Executable); err != nil && !errors.Is(err, machineguard.ErrTrustPending) {
		return hostInstallFailure("install Paperboat machine guard", readyErr, err)
	}
	return nil
}
