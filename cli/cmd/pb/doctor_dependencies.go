package main

import (
	"context"
	"os"
	"strings"
	"time"

	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	helperconfig "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
)

// doctorDependencies are the edges pb doctor reads that tests replace: the host service and machine guard live at
// fixed system paths, not under HOME.
type doctorDependencies struct {
	// hostd checks the native host supervisor.
	hostd func() []doctorpkg.Probe
	// privilegedRepair is what --repair does before the checks: nothing on Linux and macOS, the elevated repair of a
	// paired installation on Windows.
	privilegedRepair func(context.Context) error
	// guardStatus asks the running machine guard for its status (version and code revision).
	guardStatus func(context.Context) (machineguard.RuntimeStatus, error)
	// guardStartFailure is the reason the guard last stopped before serving.
	guardStartFailure func() string
	// renewalRefused reads whether the daemon recorded that the server refused this machine's identity renewal (401).
	renewalRefused func() bool
}

func defaultDoctorDependencies() doctorDependencies {
	return doctorDependencies{
		hostd:             doctorHostdProbes,
		privilegedRepair:  platformDoctorRepair,
		guardStatus:       readRunningMachineGuardStatus,
		guardStartFailure: machineguard.StartFailure,
		renewalRefused:    renewalRefusedInState,
	}
}

func doctorDependenciesOf(ctx context.Context) doctorDependencies {
	return dependenciesIn(ctx, defaultDoctorDependencies)
}

func readRunningMachineGuardStatus(ctx context.Context) (machineguard.RuntimeStatus, error) {
	client, err := machineguard.Connect(ctx, machineguard.DefaultSocket)
	if err != nil {
		return machineguard.RuntimeStatus{}, err
	}
	defer client.Close()
	return client.Status(ctx)
}

func renewalRefusedInState() bool {
	root := strings.TrimSpace(os.Getenv("PAPERBOAT_RUNTIME_STATE_ROOT"))
	if root == "" {
		var err error
		if root, err = helperconfig.DefaultStateRoot(os.Getenv); err != nil {
			return false
		}
	}
	return enrollment.RenewalRefused(root, time.Now().UTC())
}
