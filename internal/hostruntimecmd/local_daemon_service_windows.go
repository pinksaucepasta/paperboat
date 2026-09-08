//go:build windows

package hostruntimecmd

import (
	"context"
	"errors"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func runLocalDaemonService(_ context.Context, args []string, _ io.Writer, _ io.Writer) error {
	if len(args) != 0 {
		return errors.New("local daemon service does not accept arguments")
	}
	install, err := windowsRuntimeInstallConfig()
	if err != nil {
		return err
	}
	layout, err := service.DefaultLayout("windows")
	if err != nil {
		return err
	}
	return service.RunWindowsService(localDaemonServiceConfig(install, layout.Binary))
}

func localDaemonServiceConfig(install hostinstall.WindowsRuntimeConfig, executable string) service.ServiceEntryConfig {
	return service.ServiceEntryConfig{
		Name:        "PaperboatLocalDaemon",
		Executable:  executable,
		Arguments:   []string{"daemon", "--server", install.ControlURL},
		EnrolledSID: install.OwnerSID,
		LaunchFailure: func(err error) {
			recordWindowsServiceLaunchFailure("PaperboatLocalDaemon", err)
		},
	}
}

func executeLocalDaemonService(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if err := runLocalDaemonService(ctx, args, stdout, stderr); err != nil {
		writeError(stderr, err)
		return 1
	}
	return 0
}
