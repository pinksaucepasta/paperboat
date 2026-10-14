package sessionowner

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/session"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/store"
	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workloadbridge"
)

// NewArchiveReader runs inside the current account-user owner. Root hostd
// requests only a protected generation and never opens account-controlled DBs.
func NewArchiveReader(root string, active Descriptor, maximum int) (func(context.Context, string) (workloadbridge.ArchiveMetadata, error), error) {
	if !absolute(root) || active.Validate() != nil || maximum < 1 || !currentAccount(active) {
		return nil, ErrInvalid
	}
	declared, err := Read(filepath.Join(root, active.Generation+".json"))
	if err != nil || declared != active {
		return nil, errors.Join(ErrInvalid, err)
	}
	// Metadata reads are serialized independently of live terminal operations.
	admission := make(chan struct{}, 1)
	return func(ctx context.Context, generation string) (_ workloadbridge.ArchiveMetadata, resultErr error) {
		out := workloadbridge.ArchiveMetadata{Generation: generation}
		if ctx == nil || !ValidGeneration(generation) || generation == active.Generation {
			return out, ErrInvalid
		}
		select {
		case admission <- struct{}{}:
			defer func() { <-admission }()
		case <-ctx.Done():
			return out, ctx.Err()
		}
		d, err := Read(filepath.Join(root, generation+".json"))
		if err != nil {
			return out, err
		}
		if d.Generation != generation || !sameAccount(d, active) {
			return out, ErrInvalid
		}
		unlock, err := LockStore(d)
		if errors.Is(err, ErrBusy) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		defer func() { resultErr = errors.Join(resultErr, unlock()) }()
		durable, err := store.OpenReadOnly(ctx, d.StoreRoot())
		if err != nil {
			return out, err
		}
		defer func() { resultErr = errors.Join(resultErr, durable.Close()) }()
		records, err := durable.BoundedSessionMetadata(ctx, maximum, workloadbridge.MaxBodyBytes)
		if err != nil {
			return out, err
		}
		snapshots := make([]session.Snapshot, 0, len(records))
		for _, record := range records {
			snapshot, err := session.StoppedSnapshot(record)
			if errors.Is(err, session.ErrUpdateBusy) {
				return out, nil
			}
			if err != nil {
				return out, err
			}
			snapshots = append(snapshots, snapshot)
		}
		out.Available, out.Snapshots = true, snapshots
		return out, nil
	}, nil
}
