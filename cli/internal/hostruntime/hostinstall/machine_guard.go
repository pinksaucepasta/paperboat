//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"strings"
	"syscall"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/config"

	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/service"

	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/cli/internal/selfupdate"
)

// Per-user installation reuses an authenticated ready machine-wide guard.
// Explicit guard installation remains responsible for upgrading its executable.
var existingMachineGuardReady = readyMachineGuard
var prepareMachineGuardForInstall = prepareMachineGuardFoundation

// units are the installed build's own guard units (nil: this build's).
var installMissingMachineGuard = func(ctx context.Context, executable, domain string, units *machineguard.UnitSet) (guardUndo, error) {
	undo, err := machineguard.Install(ctx, executable, domain, units)
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
// an older build must not downgrade it under another user (Windows test machine: a second Windows user's release
// install replaced the fleet's newer guard). Newer guards stay compatible with older clients, which
// send a request field only when the guard lists its feature. Prior art: Windows Installer does not
// downgrade a shared component unless told to (MSI component rules / file versioning rules).
func readyMachineGuard(ctx context.Context, version string, revision int) (resultErr error) {
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
	if !MachineGuardNotOlder(status, version, revision) {
		return fmt.Errorf("machine guard runs %s (revision %d), not the guard of this installation (%s, revision %d)", status.Version, status.Revision, version, revision)
	}
	return nil
}

// MachineGuardNotOlder (also pb doctor's mismatch test) reports whether a running guard may stay for the build
// of the given version whose guard code is revision (the build being installed, not the running process: an
// older pb installing a newer build must judge the guard against the new build). It is true for the same
// version or a release not older. Development versions do not order, so a development build keeps a guard whose
// revision is not lower than its own (never a downgrade of the shared guard; 2026-11-27: pb 2026.11.26.21 updating
// to a dev build of a higher revision kept its own guard because it compared with its own revision), and a release keeps a development guard only when that guard
// carries newer guard code (canaries 2026-11-26: custom -> official kept the custom build's guard). Revision 0
// (the candidate's revision is unknown: Windows, the heal, a candidate built before it reported one) keeps only
// the same version or a release not older, so an unknown build never keeps a guard it cannot prove is its own.
func MachineGuardNotOlder(status machineguard.RuntimeStatus, version string, revision int) bool {
	if status.Version == version {
		return true
	}
	if order, err := selfupdate.CompareVersions(status.Version, version); err == nil {
		return order >= 0
	}
	if revision == 0 {
		return false
	}
	if _, err := selfupdate.CompareVersions(version, version); err == nil {
		return status.Revision > revision
	}
	return status.Revision >= revision
}

// candidateGuardRevision is the guard revision of the build being installed: this process's for its own
// units (nil), else the units' own, 0 when they do not say (never the installer's: it may be another build).
func candidateGuardRevision(units *machineguard.UnitSet) int {
	if units == nil {
		return machineguard.Revision
	}
	return units.Revision
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
func installMachineGuard(ctx context.Context, request Request, units *machineguard.UnitSet) error {
	undo, err := installMachineGuardReversible(ctx, request, units)
	finishMachineGuard(ctx, undo, nil)
	return err
}

// installMachineGuardReversible installs the guard when the existing one is not ready and returns the
// Undo that restores the replaced guard (nil when the existing guard was kept). An update restores it
// if the update fails after this step, so no component is left on the new version.
func installMachineGuardReversible(ctx context.Context, request Request, units *machineguard.UnitSet) (guardUndo, error) {
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
	readyErr := existingMachineGuardReady(probeCtx, request.Source.Version, candidateGuardRevision(units))
	cancel()
	if readyErr == nil {
		return nil, nil
	}
	log.Printf("paperboat: replacing machine guard: %v", readyErr)
	// A guard that is simply not running (fresh machine) is expected and is not a cause of a later failure.
	if machineGuardNotRunning(readyErr) {
		readyErr = nil
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
	undo, err := installMissingMachineGuard(ctx, request.Executable, domain, units)
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
		return service.RollbackIncomplete(hostInstallFailure("restore the previous Paperboat machine guard", err))
	}
	return nil
}

// machineGuardNotRunning reports a probe miss that only means no guard is serving yet. ECONNREFUSED is a
// Unix socket with no listener; Windows' missing named pipe is covered by ErrNotExist.
func machineGuardNotRunning(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED)
}

// ErrNoServerURL is returned when an installation has no Paperboat API URL.
var ErrNoServerURL = errors.New("this pb build has no Paperboat server URL; run pb with --server https://<your Paperboat API host>")

// ValidateControlURL fails early, before any service is touched, when the control URL is empty or invalid.
func ValidateControlURL(raw string) error {
	if strings.TrimSpace(raw) == "" {
		return ErrNoServerURL
	}
	if _, err := config.NormalizeServerURL(raw); err != nil {
		return fmt.Errorf("invalid Paperboat server URL; run pb with --server https://<your Paperboat API host>: %w", err)
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
