package hostinstall

import (
	"context"
)

type localDaemonActivation struct {
	Installed bool
	Start     func(context.Context) error
	// Install is nil for a caller that only starts the service (the updater).
	Install   func(context.Context) error
	WaitReady func(context.Context) error
}

func activateLocalDaemon(ctx context.Context, activation localDaemonActivation) error {
	if ctx == nil || activation.Start == nil || activation.WaitReady == nil {
		return ErrInvalidRequest
	}
	if !activation.Installed && activation.Install == nil {
		// pb service uninstall removed the service on purpose. As on Linux and macOS, only
		// pb service install and the installer bring it back.
		return nil
	}
	var err error
	if activation.Installed {
		err = activation.Start(ctx)
	} else {
		err = activation.Install(ctx)
	}
	if err != nil {
		return err
	}
	return activation.WaitReady(ctx)
}
