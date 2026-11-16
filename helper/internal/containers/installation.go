package containers

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/pinksaucepasta/paperboat/relay/selfhost"
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
	images, err := LoadImages(dir)
	if err != nil {
		return err
	}
	// A handover keeps the process epoch, so the epoch cannot show which image serves: the
	// container under the role's name must run the installed image.
	want := map[string]string{}
	for role := range clients {
		image, err := images.Image(role)
		if err != nil {
			return err
		}
		if want[role], err = resolvedImage(ctx, image); err != nil {
			return err
		}
	}
	owner := installationOwner(dir)
	probes := map[string]func(context.Context) (NodeReadiness, error){}
	for role, client := range clients {
		probes[role] = client.readiness
	}
	return pollInstallationReady(ctx, owner, want, previous, inspectContainer, probes)
}

var errForeignContainer = errors.New("runtime container belongs to another installation")

// pollInstallationReady retries every inspect or readiness failure until ctx ends: during a
// handover the old process resets connections and a loaded host answers slowly, and one failed
// poll must not roll the upgrade back (prior art: Kubernetes readiness probes only mark a pod
// not ready until failureThreshold, Docker HEALTHCHECK --retries). Only a foreign container is
// final. On timeout the last failure is returned with ctx.Err().
func pollInstallationReady(ctx context.Context, owner string, want, previous map[string]string,
	inspect func(context.Context, string) (containerInfo, bool, error),
	probes map[string]func(context.Context) (NodeReadiness, error)) error {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		ready := true
		for role, probe := range probes {
			info, exists, err := inspect(ctx, containerName(owner, role))
			if err == nil && exists && info.Owner != owner {
				return errForeignContainer
			}
			if err != nil {
				last, ready = fmt.Errorf("%s: %w", role, err), false
				continue
			}
			if !exists || !info.Running || info.ImageID != want[role] {
				ready = false
				continue
			}
			state, err := probe(ctx)
			if err != nil {
				last, ready = fmt.Errorf("%s readiness: %w", role, err), false
				continue
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
			if last != nil {
				return fmt.Errorf("installation not ready: %w (last failure: %w)", ctx.Err(), last)
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
