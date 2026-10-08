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

func readyMachineGuard(ctx context.Context) error {
	client, err := machineguard.Connect(ctx, machineguard.DefaultSocket)
	if err != nil {
		return err
	}
	defer client.Close()
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
	if !status.TrustReady {
		return errors.New("machine guard HTTPS certificate trust is not ready; retry machine-guard installation")
	}
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
		return fmt.Errorf("verify Paperboat machine guard executable: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	readyErr := existingMachineGuardReady(probeCtx)
	cancel()
	if readyErr == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := prepareMachineGuardForInstall(); err != nil {
		return fmt.Errorf("prepare Paperboat machine guard directory: %w", err)
	}
	if err := installMissingMachineGuard(ctx, request.Executable); err != nil {
		return fmt.Errorf("install Paperboat machine guard: %w", err)
	}
	return nil
}
