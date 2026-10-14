//go:build darwin || linux || windows

package runtime

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	runtimeconfig "github.com/pinksaucepasta/paperboat/internal/hostruntime/config"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/envinject"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/hostdproto"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/pty"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/sessionowner"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
)

// IndependentSessionOwner has native service custody separate from hostd.
// The account's terminal store and PTYs have exactly one process owner.
type IndependentSessionOwner struct {
	sessions     *session.Manager
	durable      *store.Store
	unlock       func() error
	bridge       *workloadbridge.Server
	descriptor   sessionowner.Descriptor
	discardEmpty atomic.Bool
}

func NewIndependentSessionOwner(ctx context.Context, d sessionowner.Descriptor, catalogRoot string) (_ *IndependentSessionOwner, resultErr error) {
	if err := d.Validate(); err != nil {
		return nil, err
	}
	if err := validateMachineWorkspace(d.Workspace); err != nil {
		return nil, err
	}
	if _, err := validatedMachineShell(d.Shell); err != nil {
		return nil, err
	}
	if err := d.Source.Verify(d.Executable); err != nil {
		return nil, err
	}
	token, err := sessionowner.ReadCapability(d)
	if err != nil {
		return nil, err
	}
	unlock, err := sessionowner.LockStore(d)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, unlock())
		}
	}()
	durable, err := store.Open(ctx, store.Config{Root: d.StoreRoot()})
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, durable.Close())
		}
	}()
	adapter, err := pty.NewShellAdapter(d.Workspace)
	if err != nil {
		return nil, err
	}
	r := runtimeconfig.DefaultResources
	archiveReader, err := sessionowner.NewArchiveReader(catalogRoot, d, r.MaxSessions)
	if err != nil {
		return nil, err
	}
	manager, err := session.NewManager(session.ManagerConfig{
		LaunchContext: func(ctx context.Context, c pty.Command) (session.PTYProcess, error) {
			var err error
			c.Env, err = envinject.Merge(c.Env, session.LaunchEnvironment(ctx))
			if err != nil {
				return nil, err
			}
			return adapter.Start(c)
		},
		Random: rand.Reader, HistoryBytes: r.HistoryBytes,
		AttachmentBytes: runtimeconfig.DefaultLimits.PendingOutputBytes,
		MaxSessions:     r.MaxSessions, MaxAttachments: r.MaxAttachments, MaxInputDecisions: r.MaxInputDecisions,
		TerminationTimeout: 10 * time.Second, TerminationGrace: 2 * time.Second,
		Store: durable, PreserveStoppedHistory: true,
	})
	if err != nil {
		return nil, err
	}
	owner := &IndependentSessionOwner{sessions: manager, durable: durable, unlock: unlock, descriptor: d}
	owner.bridge = &workloadbridge.Server{
		ArchiveMetadata: archiveReader,
		Endpoint:        d.Endpoint, Token: token, Sessions: manager, SessionOnly: true,
		Identity: &workloadbridge.OwnerIdentity{Schema: sessionowner.Schema, Generation: d.Generation, ExecutableSHA256: d.Source.SHA256},
		Fence: func() hostdproto.Status {
			return hostdproto.Status{State: hostdproto.StateActive, WorkerID: d.Generation, Epoch: 1, APIVersion: 1}
		},
		AcquireActive: func(worker string, epoch uint64) (func(), error) {
			if worker != d.Generation || epoch != 1 {
				return nil, hostdproto.ErrFenced
			}
			return func() {}, nil
		}, Retire: func(ctx context.Context) error {
			if err := manager.Retire(ctx); err != nil {
				return err
			}
			owner.discardEmpty.Store(len(manager.List()) == 0)
			return nil
		}, Ready: make(chan error, 1), MaximumConcurrent: 256,
	}
	return owner, nil
}

func (o *IndependentSessionOwner) Ready() <-chan error           { return o.bridge.Ready }
func (o *IndependentSessionOwner) Run(ctx context.Context) error { return o.bridge.Run(ctx) }

// Close follows native service termination. Normal update retirement first
// confirms quiescence over authenticated IPC; explicit uninstall can terminate
// the service's remaining shells as part of removing Paperboat.
func (o *IndependentSessionOwner) Close(ctx context.Context) error {
	if err := errors.Join(o.sessions.Shutdown(ctx), o.durable.Close(), o.unlock()); err != nil {
		return err
	}
	if !o.discardEmpty.Load() {
		return nil
	}
	// Cleanup runs as the enrolled account and touches only this empty owner's
	// known files. The initial database also contains exec state and is retained.
	names := []string{"capability", "owner.lock"}
	if !o.descriptor.InitialStore {
		names = append(names, "state.db", "state.db-wal", "state.db-shm")
	}
	var result error
	for _, name := range names {
		err := os.Remove(filepath.Join(o.descriptor.PrivateRoot(), name))
		if !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	if result != nil {
		return result
	}
	entries, err := os.ReadDir(o.descriptor.PrivateRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		return os.Remove(o.descriptor.PrivateRoot())
	}
	return nil
}
