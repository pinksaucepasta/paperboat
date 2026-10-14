//go:build linux || darwin || windows

package hostinstall

import (
	"context"
	"errors"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"os"
	"time"
)

func startPreparedSessionOwner(ctx context.Context, c sessionowner.Catalog, d sessionowner.Descriptor) error {
	readyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := c.Install(readyCtx, d); err != nil {
		return err
	}
	client, err := sessionowner.WaitReady(readyCtx, d)
	if err != nil {
		return hostInstallFailure("session owner did not become ready", err)
	}
	client.Close()
	return nil
}

// RetireSessionOwners preserves archived data. Force is reserved for explicit
// uninstall; ordinary retirement requires process-free authenticated custody.
func RetireSessionOwners(ctx context.Context, request Request, force bool) error {
	if ctx == nil {
		return ErrInvalidRequest
	}
	c, instance, err := sessionOwnerCatalog(request)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(c.Root); installNotExistOnly(err) {
		return nil
	} else if err != nil {
		return err
	}
	descriptors, err := c.ReadAll(ctx, instance)
	if err != nil {
		return err
	}
	var result error
	for _, d := range descriptors {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		if force {
			installer, err := c.InstallerForRemoval(d)
			if err == nil {
				err = installer.Uninstall(ctx)
			}
			result = hostInstallFailure("session owner uninstall failed", result, err)
			continue
		}
		controller, err := service.SessionOwnerController(d.Source.Platform, d.Instance, d.Generation, d.UID, c.Runner)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		installer, err := c.InstallerForRemoval(d)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		native, ok := controller.(service.NativeLifecycleController)
		if !ok {
			return sessionowner.ErrInvalid
		}
		status, err := native.Inspect(ctx, installer.DefinitionPath())
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if !status.Registered && !status.Running {
			continue
		}
		client, err := sessionowner.Connect(ctx, d)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		retireErr := c.Retire(ctx, d, client.Retire)
		client.Close()
		result = hostInstallFailure("session owner retirement failed", result, retireErr)
	}
	return result
}

func SessionOwnerRecorded(ctx context.Context, request Request) (bool, error) {
	c, instance, err := sessionOwnerCatalog(request)
	if err != nil {
		return false, err
	}
	recorded, err := c.HasSource(ctx, instance, request.Source)
	if err != nil {
		return false, err
	}
	if request.SessionOwnerGeneration != "" {
		if request.SessionOwnerGeneration != request.Source.SHA256[:32] || !recorded {
			return false, sessionowner.ErrInvalid
		}
		return true, nil
	}
	// A legacy metadata record cannot silently become authority for an owner
	// descriptor published by a different or interrupted native transition.
	if recorded {
		return false, sessionowner.ErrInvalid
	}
	return false, nil
}

func bindInitialSessionStore(ctx context.Context, c sessionowner.Catalog, d sessionowner.Descriptor, enrolledGeneration string) (sessionowner.Descriptor, error) {
	if _, err := os.Lstat(c.Root); installNotExistOnly(err) {
		if enrolledGeneration != "" {
			return d, sessionowner.ErrInvalid
		}
		if _, err := c.HasSource(ctx, d.Instance, d.Source); err != nil {
			return d, err
		}
		d.InitialStore = true
		return d, nil
	} else if err != nil {
		return d, err
	}
	all, err := c.ReadAll(ctx, d.Instance)
	if err != nil {
		return d, err
	}
	for _, existing := range all {
		if existing.Generation == d.Generation {
			if !existing.Source.SameArtifact(d.Source) {
				return d, sessionowner.ErrInvalid
			}
			d.InitialStore = existing.InitialStore
			d.Source = existing.Source
			d.Executable = existing.Executable
			return d, nil
		}
	}
	if len(all) == 0 {
		if enrolledGeneration != "" {
			return d, sessionowner.ErrInvalid
		}
		d.InitialStore = true
	}
	return d, nil
}
