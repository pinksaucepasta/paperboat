//go:build windows

package hostruntimecmd

import (
	"context"
	"io"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
)

func runLocalDaemonService(_ context.Context, args []string, _ io.Writer, _ io.Writer) error {
	instance, err := resolveWindowsRuntimeInstance(args)
	if err != nil {
		return err
	}
	return service.RunWindowsService(localDaemonServiceConfig(instance.config, instance.layout.Binary))
}

func localDaemonServiceConfig(install hostinstall.WindowsRuntimeConfig, executable string) service.ServiceEntryConfig {
	return service.ServiceEntryConfig{
		Name:        windowsInstanceServiceName("PaperboatLocalDaemon", install.Instance),
		Executable:  executable,
		Arguments:   []string{"daemon", "--server", install.ControlURL},
		EnrolledSID: install.OwnerSID,
		LaunchFailure: func(err error) {
			recordWindowsServiceLaunchFailure(windowsInstanceServiceName("PaperboatLocalDaemon", install.Instance), err)
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
