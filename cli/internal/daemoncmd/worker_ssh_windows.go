//go:build windows

package daemoncmd

import (
	"context"
	"os"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostinstall"
	"github.com/pinksaucepasta/paperboat/internal/localapi"
	"github.com/pinksaucepasta/paperboat/internal/localdaemon"
)

// runLocalDaemon starts the user daemon. On Windows the managed SSH sessions are bridged by the hostd worker, a
// separate process: the daemon asks its hostd for that count so /v1/connections (and the update guards reading it)
// includes them, as the Unix updater service does.
func runLocalDaemon(ctx context.Context, config localdaemon.DaemonConfig) error {
	config.WorkerOpenSSH = workerOpenSSH
	return localdaemon.Run(ctx, config)
}

func workerOpenSSH(ctx context.Context) (int, error) {
	sid, err := localdaemon.CurrentUserSID()
	if err != nil {
		return 0, err
	}
	instance, err := hostinstall.WindowsInstanceForSID(sid)
	if err != nil {
		return 0, err
	}
	layout, err := hostinstall.WindowsLayoutForInstance(instance)
	if err != nil {
		return 0, err
	}
	tokenPath, err := hostinstall.WindowsInstanceTokenPath(instance)
	if err != nil {
		return 0, err
	}
	token, err := os.ReadFile(tokenPath)
	if err != nil {
		return 0, err
	}
	defer clear(token)
	client, err := hostdproto.NewClient(layout.HostdSocket, token, localapi.WorkerOpenSSHTimeout, sid)
	if err != nil {
		return 0, err
	}
	return client.OpenSSH(ctx)
}
