//go:build darwin || linux

package daemoncmd

import (
	"encoding/json"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/updated"
	"runtime"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
	"github.com/spf13/cobra"
)

func init() { platformUpdateProbeCommand = updateProbeCommand }

func updateProbeCommand() *cobra.Command {
	var restart bool
	command := &cobra.Command{Use: "__update-probe", Hidden: true, Args: cobra.NoArgs, RunE: func(command *cobra.Command, _ []string) error {
		if restart {
			return localdaemon.RestartCurrentUserForUpdate(command.Context())
		}
		probe, err := localdaemon.ProbeCurrentUserForUpdate(command.Context())
		if err != nil {
			return err
		}
		socket := "/run/paperboat-updated/control.sock"
		if runtime.GOOS == "darwin" {
			socket = "/var/run/paperboat-updated/control.sock"
		}
		client, err := updated.NewClient(socket, time.Second)
		if err != nil {
			return err
		}
		if status, err := client.Status(command.Context()); err == nil {
			probe.UpdaterVersion = status.UpdaterVersion
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(probe)
	}}
	command.Flags().BoolVar(&restart, "restart", false, "restart the enrolled user's daemon")
	return command
}
