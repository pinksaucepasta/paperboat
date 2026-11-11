package hostruntimecmd

import (
	"context"
	"errors"
	"time"
)

// hostServiceFollowInterval is how often a Windows hostd generation's service parent checks whether the installed
// configuration still names it.
var hostServiceFollowInterval = time.Second

// followInstalledGeneration runs serve only while installed reports this generation (socket, its hostd pipe from
// the installed configuration). During an update the previous and the new hostd generation run side by side; the
// account's one host-service pipe and its power policy move with the configuration: the old parent stops serving
// once the configuration names the new generation, and the new one serves (its bind waits for the name). A
// configuration that cannot be read changes nothing. It returns when ctx ends or serve fails.
func followInstalledGeneration(ctx context.Context, socket string, installed func() (string, error), serve func(context.Context) error) error {
	stop := func() {}
	defer func() { stop() }()
	var served chan error
	halt := func() error {
		stop()
		err := <-served
		served = nil
		// The release itself can fail (restoring the power policy); only the cancellation is expected.
		if runtimeCausesOnly(err, context.Canceled) {
			return nil
		}
		return err
	}
	check := time.NewTicker(hostServiceFollowInterval)
	defer check.Stop()
	for {
		if current, err := installed(); err == nil {
			switch {
			case current == socket && served == nil:
				runCtx, cancel := context.WithCancel(ctx)
				stop = cancel
				served = make(chan error, 1)
				go func() { served <- serve(runCtx) }()
			case current != socket && served != nil:
				if err := halt(); err != nil {
					return err
				}
			}
		}
		select {
		case <-ctx.Done():
			if served != nil {
				return errors.Join(ctx.Err(), halt())
			}
			return ctx.Err()
		case err := <-served:
			stop()
			return errors.Join(errors.New("the Windows host service stopped"), err)
		case <-check.C:
		}
	}
}
