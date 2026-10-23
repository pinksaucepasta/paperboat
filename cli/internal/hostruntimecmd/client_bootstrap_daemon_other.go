//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

func bootstrapLocalDaemonInstaller(cfg *config.Config) func(context.Context) error {
	return func(ctx context.Context) error {
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		return localdaemon.InstallCurrentUserService(ctx, executable, cfg.Path(), cfg.ServerURL)
	}
}

// daemonBindWindow is how long pairing waits for the installed daemon: its service cutover and start,
// then its first refresh, which on a slow control plane uses its whole budget. A fixed 45 s ended 2 s
// before a daemon on the shared account published its first state (lanep-r3b, 2026-10-22).
const daemonBindWindow = 30*time.Second + localdaemon.FirstRefreshBudget

// bindBootstrapDaemon completes canonical cutover only after the managed runtime
// is committed. A failure retains that usable installation and the bootstrap
// resume record so setup can retry daemon readiness without reenrollment.
func bindBootstrapDaemon(ctx context.Context, serverURL, expectedVersion string) error {
	cfg, err := config.Load("")
	if err != nil {
		return err
	}
	restore, err := localdaemon.ReplaceCurrentUserService(ctx, systemWorkerExecutable(), cfg.Path(), serverURL)
	if err != nil {
		var cleanupErr error
		if restore != nil {
			recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			cleanupErr = restore(recovery)
		}
		return withCleanup("restoring the previous local daemon after enrollment binding failed", runtimeFailure("binding the local daemon to the completed enrollment failed", err), cleanupErr)
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	var lastProbe localdaemon.UpdateProbe
	var lastErr error
	for {
		probe, err := localdaemon.ProbeCurrentUserForUpdate(ctx)
		if err == nil && boundDaemonServing(probe, expectedVersion) {
			return nil
		}
		if ctx.Err() == nil {
			lastProbe, lastErr = probe, err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("installed daemon readiness probe failed: %w; retry enrollment: %w", lastErr, ctx.Err())
			}
			return fmt.Errorf("installed daemon did not become ready at version %s (running=%t state=%q version=%q); retry enrollment: %w", expectedVersion, lastProbe.Running, lastProbe.State, lastProbe.Version, ctx.Err())
		case <-ticker.C:
		}
	}
}

// boundDaemonServing reports that the installed daemon was replaced and runs the expected version for
// this enrollment: binding proves the cutover, not the control plane. A daemon still starting (its
// first refresh not published yet) or degraded only because the control plane does not answer yet is
// bound; on a slow server either can last longer than any fixed wait, and pairing reported failure on
// machines that worked (2026-10-22). The daemon's state stays visible in pb status and pb doctor. Not
// enrolled, degraded for another reason, the wrong version, or not running is not bound.
func boundDaemonServing(probe localdaemon.UpdateProbe, expectedVersion string) bool {
	if !probe.Running || probe.Version != expectedVersion {
		return false
	}
	return probe.State == "ready" || probe.State == "starting" || probe.State == "degraded" && probe.ControlPlaneUnavailableOnly
}

// daemonSettleWindow bounds how long pairing waits, after binding, for a daemon that is still starting
// to publish its first state. Binding does not wait for that (a slow control plane would fail a working
// pair), but without this wait the first command after `pb setup` or `pb pair` met a daemon that was
// still starting (C020, round 9: `pb doctor` right after setup said degraded, healthy seconds later).
const daemonSettleWindow = 60 * time.Second

// pairedMessage is what pairing prints once the daemon is bound: ready when it settled within
// daemonSettleWindow, otherwise that it is still starting and where to watch it.
func pairedMessage(ctx context.Context, probe func(context.Context) (localdaemon.UpdateProbe, error)) string {
	settleCtx, cancel := context.WithTimeout(ctx, daemonSettleWindow)
	defer cancel()
	if awaitBoundDaemonSettled(settleCtx, probe) {
		return "Paperboat machine runtime is ready."
	}
	return "Paperboat is paired; its local service is still starting. Run `pb status` to see when it is ready."
}

// awaitBoundDaemonSettled reports whether the bound daemon left "starting" (ready, or degraded with its
// own reason, which pb status and pb doctor show) before ctx ends.
func awaitBoundDaemonSettled(ctx context.Context, probe func(context.Context) (localdaemon.UpdateProbe, error)) bool {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		state, err := probe(ctx)
		if err == nil && state.Running && state.State != "" && state.State != "starting" {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
