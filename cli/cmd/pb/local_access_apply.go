package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/cli/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/cli/internal/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/cli/internal/localapi"
	"github.com/pinksaucepasta/paperboat/cli/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
	"os"
	"path/filepath"
	"time"
)

var applyLocalAccessSettings = applyLocalAccessConfig

// Provision trust before activating the daemon. Restore the previous domain
// and daemon if activation fails.
func applyLocalAccessConfig(ctx context.Context, cfg *config.Config, domain string) error {
	defaultConfig, err := config.Load("")
	if err != nil {
		return err
	}
	configPath, _ := filepath.Abs(cfg.Path())
	defaultPath, _ := filepath.Abs(defaultConfig.Path())
	if configPath != defaultPath {
		return errors.New("local browser settings must use the installed user's config path; use pb config path")
	}
	unlock, err := cfg.LockLocalAccess()
	if err != nil {
		return err
	}
	defer unlock()
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	state, err := localdaemon.InspectCurrentUserService(ctx, executable)
	if err != nil {
		return err
	}
	if !state.Installed {
		return errors.New("Paperboat is not installed; run pb install before applying local browser settings")
	}
	status, err := localAccessGuardStatus(ctx)
	if err != nil {
		return err
	}
	// The machine guard owns the domain; an empty domain keeps its current one.
	priorDomain, nextDomain := status.BrowserDomain, domain
	if nextDomain == "" {
		nextDomain = priorDomain
	}
	if priorDomain == "" || nextDomain == "" {
		return errors.New("this computer has no local access domain; run pb install again")
	}
	changedDomain := priorDomain != nextDomain
	if err = localdaemon.StopCurrentUserService(ctx, executable); err != nil {
		return err
	}
	restore := func(cause error) error {
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
		defer cancel()
		var trustErr error
		if changedDomain {
			trustErr = hostruntimecmd.ConfigureBrowserDomain(recovery, priorDomain)
		}
		startErr := localdaemon.StartCurrentUserService(recovery, executable)
		return errors.Join(cause, trustErr, startErr)
	}
	if changedDomain {
		if err = hostruntimecmd.ConfigureBrowserDomain(ctx, nextDomain); err != nil {
			return restore(err)
		}
		if err = machineguard.CleanupUserTrust(ctx); err != nil {
			return restore(fmt.Errorf("retire previous browser certificate trust: %w", err))
		}
	}
	if err = localdaemon.StartCurrentUserService(ctx, executable); err != nil {
		return restore(fmt.Errorf("activate local browser settings: %w", err))
	}
	paths, err := localdaemon.CurrentUserPaths()
	if err != nil {
		return restore(err)
	}
	client, err := localapi.NewClient(paths.SocketPath, time.Second)
	if err != nil {
		return restore(err)
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := waitForLocalAccessReady(readyCtx, client); err != nil {
		return restore(fmt.Errorf("local daemon did not apply browser settings: %w", err))
	}
	return nil
}

// Readiness follows completed protected routing, not merely a listening IPC
// socket. Offline and unenrolled installations still accept saved settings.
func waitForLocalAccessReady(ctx context.Context, client localDaemonSnapshotClient) error {
	ticker := time.NewTicker(localDaemonReadyPollInterval)
	defer ticker.Stop()
	for {
		snapshot, err := client.Snapshot(ctx)
		if err == nil && snapshot.DaemonVersion == buildinfo.Version {
			for _, health := range snapshot.Health {
				if health.Code == "local_access_unavailable" {
					return errors.New("local machine access could not be configured; check local browser settings and repair Paperboat")
				}
			}
			if snapshot.DaemonState == "ready" || snapshot.DaemonState == "awaiting_enrollment" {
				return nil
			}
			if snapshot.DaemonState == "degraded" && len(snapshot.Health) == 1 && snapshot.Health[0].Code == "control_plane_unavailable" {
				return nil
			}
		} else if err != nil && !localDaemonSocketUnavailable(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
