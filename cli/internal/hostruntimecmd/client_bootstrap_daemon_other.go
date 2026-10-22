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

// boundDaemonServing reports that the installed daemon runs the expected version and serves this
// enrollment. A daemon whose only problem is reaching the control plane is serving: on a slow server or
// a large account its first inventory refresh can fail, and pairing reported failure on a machine that
// worked (Lane A, 2026-10-22). The update baseline accepts the same state.
func boundDaemonServing(probe localdaemon.UpdateProbe, expectedVersion string) bool {
	if !probe.Running || probe.Version != expectedVersion {
		return false
	}
	return probe.State == "ready" || probe.State == "degraded" && probe.ControlPlaneUnavailableOnly
}
