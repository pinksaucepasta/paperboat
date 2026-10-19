//go:build darwin || linux || windows

package hostinstall

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"github.com/pinksaucepasta/paperboat/internal/splitdns"
)

// Per-user installation reuses an authenticated ready machine-wide guard.
// Explicit guard installation remains responsible for upgrading its executable.
var existingMachineGuardReady = readyMachineGuard
var prepareMachineGuardForInstall = prepareMachineGuardFoundation
var installMissingMachineGuard = machineguard.Install
var existingMachineGuardDomain = readMachineGuardDomain
var discoverBrowserDomain = discoverServerBrowserDomain

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

// discoverServerBrowserDomain asks the deployment's server which domain local
// access uses; the guard has no built-in one.
func discoverServerBrowserDomain(ctx context.Context, controlURL string) (string, error) {
	serverURL, err := config.NormalizeServerURL(controlURL)
	if err != nil {
		return "", err
	}
	discovery, err := api.New(serverURL, config.Credential{}, &http.Client{Timeout: 15 * time.Second}).ClientConfiguration(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(discovery.LocalAccessBaseDomain) == "" {
		return "", errors.New("the server did not publish a local access domain; contact the server operator")
	}
	return splitdns.NormalizeBrowserDomain(discovery.LocalAccessBaseDomain)
}

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
	if status.DefaultBrowserDomain == "" {
		return errors.New("machine guard has no local access domain; retry machine-guard installation")
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
	// A guard that already records its local access domain keeps it; otherwise
	// this installation learns the deployment's domain from the server.
	domainCtx, cancelDomain := context.WithTimeout(ctx, 5*time.Second)
	domain := existingMachineGuardDomain(domainCtx)
	cancelDomain()
	if domain == "" {
		discoverCtx, cancelDiscover := context.WithTimeout(ctx, 30*time.Second)
		found, err := discoverBrowserDomain(discoverCtx, request.ControlURL)
		cancelDiscover()
		if err != nil {
			return hostInstallFailure("read the local access domain from the Paperboat server; check the network and retry", readyErr, err)
		}
		domain = found
	}
	// A ready guard whose browser trust awaits approval is installed successfully.
	if err := installMissingMachineGuard(ctx, request.Executable, domain); err != nil && !errors.Is(err, machineguard.ErrTrustPending) {
		return hostInstallFailure("install Paperboat machine guard", readyErr, err)
	}
	return nil
}
