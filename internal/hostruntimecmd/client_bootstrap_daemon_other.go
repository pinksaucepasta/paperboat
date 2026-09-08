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
	if err := localdaemon.InstallCurrentUserService(ctx, systemWorkerExecutable(), cfg.Path(), serverURL); err != nil {
		return err
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		probe, err := localdaemon.ProbeCurrentUserForUpdate(ctx)
		if err == nil && probe.Running && probe.Version == expectedVersion && probe.State == "ready" {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("installed daemon did not become ready at version %s: %w", expectedVersion, ctx.Err())
		case <-ticker.C:
		}
	}
}
