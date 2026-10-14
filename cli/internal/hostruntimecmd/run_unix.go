//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/buildinfo"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/runtime"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func runProduction(ctx context.Context, output io.Writer) error {
	notifier, err := service.NewProcessNotifier()
	if err != nil {
		return err
	}
	if err := notifier.Starting(); err != nil {
		return err
	}
	host, err := runtime.NewProductionHost(ctx, buildinfo.Version, os.Getenv)
	if err != nil {
		return runtimeFailure("runtime initialization failed", err, notifier.Degraded("runtime initialization failed"))
	}
	if err := host.Start(ctx); err != nil {
		return runtimeFailure("runtime startup failed", err, notifier.Degraded("runtime startup failed"))
	}
	if err := notifier.Ready(); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		return errors.Join(err, host.Shutdown(shutdownCtx))
	}
	if _, err := fmt.Fprintln(output, "pb machine runtime ready"); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		return runtimeFailure("runtime readiness could not be written", err, host.Shutdown(shutdownCtx), notifier.Stopping())
	}
	watchdogInterval := notifier.WatchdogInterval()
	var watchdog <-chan time.Time
	var ticker *time.Ticker
	if watchdogInterval > 0 {
		ticker = time.NewTicker(watchdogInterval)
		defer ticker.Stop()
		watchdog = ticker.C
	}
	var runErr error
run:
	for {
		select {
		case <-ctx.Done():
			runErr = runtimeFailureIfAny("runtime draining failed", runtimeShutdownCause(ctx), notifier.Draining())
			break run
		case <-watchdog:
			if err := notifier.Watchdog(); err != nil {
				runErr = errors.Join(err, notifier.Degraded("watchdog notification failed"))
				break run
			}
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	return errors.Join(runErr, notifier.Stopping(), host.Shutdown(shutdownCtx))
}
