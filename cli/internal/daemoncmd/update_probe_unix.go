//go:build darwin || linux

package daemoncmd

import (
	"context"
	"encoding/json"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
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
		probe.UpdaterVersion, err = updateProbeUpdaterVersion(command.Context(), runtime.GOOS, os.Geteuid(), readUpdateProbeUpdaterVersion)
		if err != nil {
			return err
		}
		return json.NewEncoder(command.OutOrStdout()).Encode(probe)
	}}
	command.Flags().BoolVar(&restart, "restart", false, "restart the enrolled user's daemon")
	return command
}

func updateProbeUpdaterVersion(ctx context.Context, platform string, uid int, read func(context.Context, string) (string, error)) (string, error) {
	layout, err := service.UserLayout(platform, uid)
	if err != nil {
		return "", err
	}
	return read(ctx, layout.UpdaterSocket)
}

func readUpdateProbeUpdaterVersion(ctx context.Context, socket string) (string, error) {
	client, err := updated.NewClient(socket, time.Second)
	if err != nil {
		return "", err
	}
	status, err := client.Status(ctx)
	if err != nil {
		return "", err
	}
	return status.UpdaterVersion, nil
}
