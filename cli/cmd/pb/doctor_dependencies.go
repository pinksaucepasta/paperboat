package main

import (
	"context"
	"os"
	"strings"
	"time"

	"github.com/pinksaucepasta/paperboat/cli/internal/api"
	doctorpkg "github.com/pinksaucepasta/paperboat/cli/internal/doctor"
	helperconfig "github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/cli/internal/hostruntime/enrollment"
	"github.com/pinksaucepasta/paperboat/cli/internal/machineguard"
	"github.com/spf13/cobra"
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
	// machineRecord reads one machine's current record from the server.
	machineRecord func(context.Context, *cobra.Command, string) (api.UserMachine, error)
}

func defaultDoctorDependencies() doctorDependencies {
	return doctorDependencies{
		hostd:             doctorHostdProbes,
		privilegedRepair:  platformDoctorRepair,
		guardStatus:       readRunningMachineGuardStatus,
		guardStartFailure: machineguard.StartFailure,
		renewalRefused:    renewalRefusedInState,
		machineRecord:     doctorMachineRecord,
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

// doctorMachineRecord asks outside any workspace, as the daemon's machine list does: scoped to the selected one,
// the server answers 404 for a machine of another.
func doctorMachineRecord(ctx context.Context, command *cobra.Command, machineID string) (api.UserMachine, error) {
	commandContext := actionContext(command, []string{machineID})
	commandContext.Context = ctx
	d, err := buildDeps(commandContext)
	if err != nil {
		return api.UserMachine{}, err
	}
	credential, err := d.auth.Credential()
	if err != nil {
		return api.UserMachine{}, err
	}
	client := api.New(d.cfg.ServerURL, credential, nil)
	client.SetCredentialSource(d.auth)
	return client.GetUserMachine(ctx, machineID)
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
