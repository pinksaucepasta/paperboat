//go:build darwin || linux

package hostruntimecmd

import (
	"context"
	"os"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func nativeMaintenanceConfig(environ func(string) string) (bool, func(context.Context) error, error) {
	marker := environ("PAPERBOAT_NATIVE_UPDATE_PENDING")
	if marker == "" || marker == "false" {
		return false, nil, nil
	}
	if marker != "true" {
		return false, nil, hostdproto.ErrInvalidConfig
	}
	// A loaded launchd declaration can retain an old marker after commit.
	// Bind its release check to the actual executing image, not cached env.
	_, source, err := installsource.Current()
	if err != nil {
		return false, nil, err
	}
	layout, err := service.UserLayout(runtime.GOOS, os.Geteuid())
	if err != nil {
		return false, nil, err
	}
	client, err := updated.NewClient(layout.UpdaterSocket, 3*time.Second)
	if err != nil {
		return false, nil, err
	}
	return true, committedSourceVerifier(source, client), nil
}

func committedSourceVerifier(expected installsource.Source, client *updated.Client) func(context.Context) error {
	return func(ctx context.Context) error {
		response, err := client.Status(ctx)
		if err != nil {
			return err
		}
		state := response.Transaction
		if response.Status != "ok" || state.Schema != workerupdate.TransactionSchemaV1 || state.ActiveVersion != expected.Version || state.ActiveDigest != expected.SHA256 || state.ActiveLength != expected.Length {
			return hostdproto.ErrMaintenanceBusy
		}
		return ctx.Err()
	}
}

// Native readiness precedes metadata commit. Keep the same startup fence until
// the privileged updater can prove commit, including a stale loaded declaration
// after reboot. This observer never invokes an activation or restarts a service.
func startMaintenanceCompletion(ctx context.Context, server *hostdproto.Server) func() {
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for runCtx.Err() == nil {
			probeCtx, finish := context.WithTimeout(runCtx, 3*time.Second)
			err := server.CompleteMaintenance(probeCtx)
			finish()
			if err == nil {
				return
			}
			timer := time.NewTimer(time.Second)
			select {
			case <-runCtx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}()
	return func() { cancel(); <-done }
}
