package daemoncmd

import (
	"context"
	"errors"
)

// The local runtime owns its cleanup. A terminal sync failure stops it and waits
// for cleanup before returning the actionable authority error to its supervisor.
func runWithSyncFailure(ctx context.Context, failures <-chan error, run func(context.Context) error) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx) }()
	select {
	case err := <-done:
		return err
	case err := <-failures:
		cancel()
		cleanupErr := <-done
		if cleanupErr == context.Canceled {
			cleanupErr = nil
		}
		return errors.Join(err, cleanupErr)
	}
}
