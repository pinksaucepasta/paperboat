package containers

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat-relay/selfhost"
)

func InstallationEpochs(ctx context.Context, dir string) (map[string]string, error) {
	dirs, err := selfhost.ComponentDirectories(dir)
	if err != nil {
		return nil, err
	}
	epochs := map[string]string{}
	for _, componentDir := range dirs {
		role := filepath.Base(componentDir)
		client, err := newNodeClient(role, componentDir)
		if err != nil {
			return nil, err
		}
		state, err := client.readiness(ctx)
		client.http.CloseIdleConnections()
		if err != nil {
			return nil, err
		}
		epochs[role] = state.ProcessEpoch
	}
	return epochs, nil
}

// WaitInstallationReady checks every owned container and its exact authenticated
// node. A heartbeat from the pre-upgrade process cannot confirm an upgrade.
func WaitInstallationReady(ctx context.Context, dir string, previous map[string]string) error {
	dirs, err := selfhost.ComponentDirectories(dir)
	if err != nil {
		return err
	}
	clients := map[string]*nodeClient{}
	defer func() {
		for _, client := range clients {
			client.http.CloseIdleConnections()
		}
	}()
	for _, componentDir := range dirs {
		role := filepath.Base(componentDir)
		client, err := newNodeClient(role, componentDir)
		if err != nil {
			return err
		}
		clients[role] = client
	}
	if len(clients) == 0 {
		return errors.New("installation has no configured runtimes")
	}
	owner := installationOwner(dir)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready := true
		for role, client := range clients {
			info, exists, err := inspectContainer(ctx, "paperboat-pbh-"+owner+"-"+role)
			if err != nil {
				return err
			}
			if exists && info.Owner != owner {
				return errors.New("runtime container belongs to another installation")
			}
			if !exists || !info.Running {
				ready = false
				continue
			}
			state, err := client.readiness(ctx)
			if err != nil {
				return err
			}
			if !state.Ready || state.ProcessEpoch == previous[role] {
				ready = false
			}
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
