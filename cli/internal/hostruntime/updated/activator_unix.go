//go:build darwin || linux

package updated

import (
	"context"
	"errors"
	"runtime"
	"strconv"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

// FixedSupervisorActivator is the only service-manager operation the updater
// can perform. It has no caller-supplied executable, arguments, unit, or
// launchd label. The journal remains durable before these calls because the
// updater itself may be restarted by the second operation.
type FixedSupervisorActivator struct {
	Platform string
	UID      int
	Runner   service.Runner
}

func (a FixedSupervisorActivator) Activate(ctx context.Context) error {
	return a.restart(ctx)
}

func (a FixedSupervisorActivator) Rollback(ctx context.Context) error {
	return a.restart(ctx)
}

func (a FixedSupervisorActivator) restart(ctx context.Context) error {
	if a.Runner == nil || a.Platform != runtime.GOOS || a.UID < 0 {
		return errors.New("invalid fixed supervisor activator")
	}
	switch a.Platform {
	case "linux":
		instance := "u" + strconv.Itoa(a.UID)
		if err := a.Runner.Run(ctx, "systemctl", "restart", "paperboat-hostd-"+instance+".service"); err != nil {
			return err
		}
		return a.Runner.Run(ctx, "systemctl", "restart", "paperboat-updated-"+instance+".service")
	case "darwin":
		instance := "u" + strconv.Itoa(a.UID)
		if err := a.Runner.Run(ctx, "launchctl", "kickstart", "-k", "system/com.pinksaucepasta.paperboat.hostd."+instance); err != nil {
			return err
		}
		return a.Runner.Run(ctx, "launchctl", "kickstart", "-k", "system/com.pinksaucepasta.paperboat.updated."+instance)
	default:
		return errors.New("unsupported supervisor platform")
	}
}
