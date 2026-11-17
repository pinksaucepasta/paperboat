//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/cli/internal/selfupdate"
)

// Per-user installation reuses an authenticated ready machine-wide guard.
// Explicit guard installation remains responsible for upgrading its executable.
var existingMachineGuardReady = readyMachineGuard
var prepareMachineGuardForInstall = prepareMachineGuardFoundation
var installMissingMachineGuard = func(ctx context.Context, executable, domain string) (guardUndo, error) {
	undo, err := machineguard.Install(ctx, executable, domain)
	if undo == nil {
		return nil, err
	}
	return undo, err
}

// guardUndo restores (or keeps) the guard an installation replaced; machineguard.Undo implements it.
type guardUndo interface {
	Restore(context.Context) error
	Discard()
}

var existingMachineGuardDomain = readMachineGuardDomain
var discoverBrowserDomain = machineguard.DiscoverBrowserDomain

// readMachineGuardDomain is the local access domain an installed guard already
// records, or "" when there is no usable guard or it has none.
func readMachineGuardDomain(ctx context.Context) string {
	client, err := machineguard.Connect(ctx, machineguard.DefaultSocket)
	if err != nil {
		return ""
	}
	defer client.Close()
	status, err := client.Status(ctx)
	if err != nil {
		return ""
	}
	return status.DefaultBrowserDomain
}

// readyMachineGuard reports nil when the installed guard is ready and not older than version, the build
// being installed. The guard is one per machine, shared by every OS user's install: a user installing
// an older build must not downgrade it under another user (victus: a second Windows user's release
// install replaced the fleet's newer guard). Newer guards stay compatible with older clients, which
// send a request field only when the guard lists its feature. Prior art: Windows Installer does not
// downgrade a shared component unless told to (MSI component rules / file versioning rules).
func readyMachineGuard(ctx context.Context, version string) (resultErr error) {
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
	if err := validateMachineGuardStatus(status); err != nil {
		return err
	}
	if !MachineGuardNotOlder(status, version) {
		return fmt.Errorf("machine guard runs %s, older than this installation (%s)", status.Version, version)
	}
	return nil
}

// MachineGuardNotOlder (also pb doctor's mismatch test): the same version, a release not older, or (for development builds, whose
// versions do not order) a guard whose code revision is not lower than this build's.
func MachineGuardNotOlder(status machineguard.RuntimeStatus, version string) bool {
	if status.Version == version {
		return true
	}
	if order, err := selfupdate.CompareVersions(status.Version, version); err == nil {
		return order >= 0
	}
	return status.Revision >= machineguard.Revision
}

func validateMachineGuardStatus(status machineguard.RuntimeStatus) error {
	if !status.Ready {
		return errors.New("machine guard is not ready; retry machine-guard installation")
	}
	if status.RootRenewalRequired {
		return errors.New("machine guard HTTPS root requires renewal; retry machine-guard installation")
	}
	if status.DefaultBrowserDomain == "" {
		return errors.New("machine guard has no local access domain; retry machine-guard installation")
	}
	// TrustReady may be false while browser trust awaits approval; the guard is usable.
	return nil
}
func installMachineGuard(ctx context.Context, request Request) error {
	undo, err := installMachineGuardReversible(ctx, request)
	finishMachineGuard(ctx, undo, nil)
	return err
}

// installMachineGuardReversible installs the guard when the existing one is not ready and returns the
// Undo that restores the replaced guard (nil when the existing guard was kept). An update restores it
// if the update fails after this step, so no component is left on the new version.
func installMachineGuardReversible(ctx context.Context, request Request) (guardUndo, error) {
	if ctx == nil {
		return nil, ErrInvalidRequest
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.Source.Verify(request.Executable); err != nil {
		return nil, hostInstallFailure("verify Paperboat machine guard executable", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	readyErr := existingMachineGuardReady(probeCtx, request.Source.Version)
	cancel()
	if readyErr == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, hostInstallFailure("machine guard installation canceled", readyErr, err, context.Cause(ctx))
	}
	if err := prepareMachineGuardForInstall(); err != nil {
		return nil, hostInstallFailure("prepare Paperboat machine guard directory", readyErr, err)
	}
	// A guard that already records its local access domain keeps it; otherwise
	// this installation learns the deployment's domain from the server.
	domain, err := MachineGuardBrowserDomain(ctx, request.ControlURL)
	if err != nil {
		return nil, hostInstallFailure("read the local access domain from the Paperboat server; check the network and retry", readyErr, err)
	}
	// A ready guard whose browser trust awaits approval is installed successfully.
	undo, err := installMissingMachineGuard(ctx, request.Executable, domain)
	if err != nil && !errors.Is(err, machineguard.ErrTrustPending) {
		return nil, hostInstallFailure("install Paperboat machine guard", readyErr, err)
	}
	return undo, nil
}

// finishMachineGuard completes the guard part of an installation: when the installation failed it puts
// back the guard it replaced (with its own bounded context, so the caller's cancellation cannot leave
// the new guard behind) and returns the restore error; otherwise it keeps the new guard.
func finishMachineGuard(ctx context.Context, undo guardUndo, installErr error) error {
	if undo == nil {
		return nil
	}
	if installErr == nil {
		undo.Discard()
		return nil
	}
	restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	if err := undo.Restore(restoreCtx); err != nil {
		return hostInstallFailure("restore the previous Paperboat machine guard", err)
	}
	return nil
}

// MachineGuardBrowserDomain keeps the installed selection or discovers it from the configured server.
func MachineGuardBrowserDomain(ctx context.Context, controlURL string) (string, error) {
	domainCtx, cancelDomain := context.WithTimeout(ctx, 5*time.Second)
	domain := existingMachineGuardDomain(domainCtx)
	cancelDomain()
	if domain == "" {
		discoverCtx, cancelDiscover := context.WithTimeout(ctx, 30*time.Second)
		found, err := discoverBrowserDomain(discoverCtx, controlURL)
		cancelDiscover()
		if err != nil {
			return "", err
		}
		domain = found
	}
	return domain, nil
}
