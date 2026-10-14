package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntimecmd"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/pinksaucepasta/paperboat/internal/machineguard"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

var applyLocalAccessSettings = applyLocalAccessConfig

// Provision trust before publishing configuration. Restore the previous namespace
// and daemon if persistence or activation fails.
func applyLocalAccessConfig(ctx context.Context, cfg *config.Config, next config.LocalAccessConfig) error {
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
	fresh, err := config.Load(cfg.Path())
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(fresh.LocalAccess, cfg.LocalAccess) {
		return errors.New("local browser settings changed while this command was waiting; retry the command")
	}
	*cfg = *fresh
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
	guard, err := machineguard.Connect(ctx, machineguard.DefaultSocket)
	if err != nil {
		return err
	}
	status, err := guard.Status(ctx)
	_ = guard.Close()
	if err != nil {
		return err
	}
	priorDomain := status.BrowserDomain
	if priorDomain == "" {
		priorDomain = config.DefaultLocalAccessDomain
	}
	old := cloneLocalAccess(cfg.LocalAccess)
	old.Domain = priorDomain
	changedDomain := priorDomain != next.Domain
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
		cfg.LocalAccess = old
		saveErr := cfg.Save()
		startErr := localdaemon.StartCurrentUserService(recovery, executable)
		return errors.Join(cause, trustErr, saveErr, startErr)
	}
	if changedDomain {
		if err = hostruntimecmd.ConfigureBrowserDomain(ctx, next.Domain); err != nil {
			return restore(err)
		}
		if err = machineguard.CleanupUserTrust(ctx); err != nil {
			return restore(fmt.Errorf("retire previous browser certificate trust: %w", err))
		}
	}
	cfg.LocalAccess = next
	if err = cfg.Save(); err != nil {
		return restore(fmt.Errorf("save local browser settings: %w", err))
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
