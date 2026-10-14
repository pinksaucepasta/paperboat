package daemoncmd

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/api"
	"github.com/pinksaucepasta/paperboat/internal/config"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

type awaitingEnrollmentSource struct{}

func (awaitingEnrollmentSource) ListUserMachines(context.Context) ([]api.UserMachine, error) {
	return nil, config.ErrNoCredentials
}

// Keep local status available without credentials or network access. Enrollment
// owns writing the registration; observing it transitions to the ordinary daemon.
func runAwaitingEnrollment(parent context.Context, cfg *config.Config, paths localapi.Paths) error {
	ctx, stop := context.WithCancel(parent)
	defer stop()
	transition := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				transition <- ctx.Err()
				return
			case <-ticker.C:
				_, err := configuredMachineID()
				if errors.Is(err, os.ErrNotExist) {
					continue
				}
				transition <- err
				stop()
				return
			}
		}
	}()
	err := localdaemon.Run(ctx, localdaemon.DaemonConfig{Paths: paths, Source: awaitingEnrollmentSource{}, OwnerUID: os.Geteuid(), OwnerGID: os.Getegid()})
	stop()
	transitionErr := <-transition
	if parent.Err() != nil {
		return coordinatorFailure("Enrollment wait stopped; local daemon cleanup may be incomplete. Run pb status before retrying.", parent.Err(), context.Cause(parent), lifecycleCleanup(err), lifecycleCleanup(transitionErr))
	}
	if failure := lifecycleCleanup(err); failure != nil {
		return coordinatorFailure("The local daemon could not wait for enrollment; run pb doctor and retry.", failure, lifecycleCleanup(transitionErr))
	}
	return transitionErr
}
