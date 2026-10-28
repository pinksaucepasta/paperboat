package sessionowner

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/service"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
)

// Connect authenticates the store and exact registered source before use.
func Connect(ctx context.Context, d Descriptor) (*workloadbridge.Client, error) {
	return connectOwner(ctx, d, false)
}
func connectOwner(ctx context.Context, d Descriptor, allowRetired bool) (*workloadbridge.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, ownerInspectionTimeout)
	defer cancel()
	if err := d.Validate(); err != nil {
		return nil, err
	}
	token, err := ReadCapability(d)
	if err != nil {
		return nil, err
	}
	client, err := workloadbridge.NewClient(d.Endpoint, token, d.Generation, 1)
	if err != nil {
		return nil, err
	}
	identity, err := client.OwnerIdentity(ctx)
	if err == nil && (identity.Schema != Schema || identity.Generation != d.Generation || identity.ExecutableSHA256 != d.Source.SHA256) {
		err = ErrInvalid
	}
	if err == nil && identity.Retired && !allowRetired {
		err = session.ErrManagerStopped
	}
	if err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

// WaitReady is bounded native-start readiness, not a retry of a mutation.
func WaitReady(ctx context.Context, d Descriptor) (*workloadbridge.Client, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		client, err := Connect(ctx, d)
		if err == nil {
			return client, nil
		}
		if !errors.Is(err, session.ErrOwnerUnavailable) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), err)
		case <-tick.C:
		}
	}
}

// Coordinator is used only by the privileged updater/native installer. It
// accepts a generation, never a caller-provided executable, path or source.
type Coordinator struct {
	Catalog Catalog
	Active  Descriptor
}

func (c Coordinator) descriptor(ctx context.Context, generation string) (Descriptor, error) {
	if c.Active.Validate() != nil || !ValidGeneration(generation) {
		return Descriptor{}, ErrInvalid
	}
	all, err := c.Catalog.ReadAll(ctx, c.Active.Instance)
	if err != nil {
		return Descriptor{}, err
	}
	for _, d := range all {
		if d.Generation == generation {
			if d.RuntimeRoot != c.Active.RuntimeRoot || d.UID != c.Active.UID || d.GID != c.Active.GID || d.OwnerSID != c.Active.OwnerSID {
				return Descriptor{}, ErrInvalid
			}
			return d, nil
		}
	}
	return Descriptor{}, session.ErrSessionUnknown
}

func (c Coordinator) nativeState(ctx context.Context, d Descriptor) (service.NativeControllerStatus, error) {
	installer, err := c.Catalog.installer(d, true)
	if err != nil {
		return service.NativeControllerStatus{}, err
	}
	controller, err := service.SessionOwnerController(d.Source.Platform, d.Instance, d.Generation, d.UID, c.Catalog.Runner)
	if err != nil {
		return service.NativeControllerStatus{}, err
	}
	native, ok := controller.(service.NativeLifecycleController)
	if !ok {
		return service.NativeControllerStatus{}, ErrInvalid
	}
	return native.Inspect(ctx, installer.DefinitionPath())
}

func (c Coordinator) Ensure(ctx context.Context, generation string) error {
	d, err := c.descriptor(ctx, generation)
	if err != nil {
		return err
	}
	status, err := c.nativeState(ctx, d)
	if err != nil {
		return err
	}
	if status.Running {
		client, connectErr := connectOwner(ctx, d, true)
		if connectErr == nil {
			identity, identityErr := client.OwnerIdentity(ctx)
			if identityErr == nil && identity.Retired {
				retireErr := c.Catalog.Retire(ctx, d, client.Retire)
				client.Close()
				if retireErr != nil {
					return retireErr
				}
				status = service.NativeControllerStatus{}
			} else {
				client.Close()
				if identityErr != nil {
					return identityErr
				}
			}
		} else if !errors.Is(connectErr, session.ErrOwnerUnavailable) {
			return connectErr
		}
	}
	if !status.Registered && !status.Running {
		// A confirmed archive reopens on current verified code, retaining its
		// original store, terminal IDs, replay and input journal.
		d.Source = c.Active.Source
		d.Executable = c.Active.Executable
	}
	if err := c.Catalog.Install(ctx, d); err != nil {
		return err
	}
	client, err := WaitReady(ctx, d)
	if err != nil {
		return err
	}
	client.Close()
	return nil
}

// Reconcile removes unused old native owners. Busy shells remain untouched;
// stopped history remains in its store and never requires the old process.
func (c Coordinator) Reconcile(ctx context.Context) error {
	all, err := c.Catalog.ReadAll(ctx, c.Active.Instance)
	if err != nil {
		return err
	}
	var result error
	for _, d := range all {
		if d.Generation == c.Active.Generation {
			continue
		}
		status, err := c.nativeState(ctx, d)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		if !status.Registered && !status.Running {
			// A crash may have happened after native removal but before
			// catalogue bookkeeping. Read history through the current owner;
			// privileged reconciliation never opens account-owned stores.
			active, err := Connect(ctx, c.Active)
			if err != nil {
				result = errors.Join(result, err)
				continue
			}
			metadata, err := active.ReadArchiveMetadata(ctx, d.Generation)
			active.Close()
			if err == nil && !metadata.Available {
				err = ErrBusy
			}
			if err == nil {
				err = c.Catalog.cleanRetiredNative(ctx, d)
			}
			if err == nil {
				err = c.recordRetired(ctx, d, uint64(len(metadata.Snapshots)))
			}
			result = errors.Join(result, err)
			continue
		}
		// A registered KeepAlive job can outlive retirement's IPC cleanup.
		// Restore its exact private endpoint before authenticating quiescence.
		if err := PrepareNativeEndpoint(d); err != nil {
			result = errors.Join(result, err)
			continue
		}
		client, err := connectOwner(ctx, d, true)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		var retained uint64
		err = c.Catalog.Retire(ctx, d, func(ctx context.Context) error {
			if err := client.Retire(ctx); err != nil {
				return err
			}
			identity, err := client.OwnerIdentity(ctx)
			if err != nil {
				return err
			}
			if !identity.Retired {
				return ErrInvalid
			}
			retained = identity.RetainedSessions
			return nil
		})
		client.Close()
		if err == session.ErrUpdateBusy {
			continue
		}
		if err == nil {
			err = c.recordRetired(ctx, d, retained)
		}
		result = errors.Join(result, err)
	}
	return result
}

func (c Coordinator) recordRetired(ctx context.Context, d Descriptor, retained uint64) error {
	if retained == 0 {
		return c.Catalog.Forget(ctx, d)
	}
	d.Source, d.Executable = c.Active.Source, c.Active.Executable
	return c.Catalog.Publish(ctx, d)
}

func connectedOwner(d Descriptor, client *workloadbridge.Client) Owner {
	return Owner{Descriptor: d, Identity: workloadbridge.OwnerIdentity{Schema: Schema, Generation: d.Generation, ExecutableSHA256: d.Source.SHA256}, Call: func(ctx context.Context, op string, body json.RawMessage) (json.RawMessage, error) {
		return client.Call(ctx, "session", op, body)
	}}
}
