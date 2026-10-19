package configsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

func (r *PlaintextWorkspaceReconciler) Diagnostics() ReconciliationDiagnostics {
	r.mu.Lock()
	defer r.mu.Unlock()
	return ReconciliationDiagnostics{
		Skipped: append([]PathSummary(nil), r.diagnostics.Skipped...), Conflicts: append([]PathSummary(nil), r.diagnostics.Conflicts...),
		ManifestHealth: r.diagnostics.ManifestHealth, ManifestRevision: r.diagnostics.ManifestRevision,
		ManagedPathCount: r.diagnostics.ManagedPathCount, PendingCleanPathCount: r.diagnostics.PendingCleanPathCount,
		LastAppliedRevision: r.diagnostics.LastAppliedRevision, LastPublishedRevision: r.diagnostics.LastPublishedRevision,
	}
}

func (r *PlaintextWorkspaceReconciler) CurrentManifest() Manifest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.manifest.Clone()
}

func (r *PlaintextWorkspaceReconciler) PublicationCommitted(ctx context.Context, prepared PreparedPublication, revision string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil || r.pending.Value.MappingRevision != r.mapping.Revision() || prepared.CommitID == "" ||
		r.pending.CommitID != prepared.CommitID || revision != prepared.CommitID {
		return ErrBaselineInvalid
	}
	if len(r.pending.Resolutions) > 0 {
		if err := writeResolutionCommit(
			resolutionCommitPath(r.stateRoot),
			resolutionCommit{
				Format: "paperboat-config-resolution-commit-v1", CommitID: revision,
				Baseline: r.pending.Value, Resolutions: r.pending.Resolutions,
			},
		); err != nil {
			return err
		}
	}
	err := WriteBaseline(r.baselinePath, r.pending.Value)
	if err == nil {
		for _, resolution := range r.pending.Resolutions {
			if r.resolutions != nil {
				if acknowledgeErr := r.resolutions.Acknowledge(ctx, resolution.ID, revision); acknowledgeErr != nil {
					return acknowledgeErr
				}
			}
			_ = os.RemoveAll(filepath.Join(r.stateRoot, "conflicts", resolution.ConflictRevision))
		}
		if len(r.pending.Resolutions) > 0 {
			if removeErr := os.Remove(resolutionCommitPath(r.stateRoot)); removeErr != nil {
				return removeErr
			}
		}
		r.pending = nil
	}
	return err
}

func (r *PlaintextWorkspaceReconciler) PublicationPrepared(_ context.Context, prepared PreparedPublication) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending == nil || r.pending.Value.MappingRevision != r.mapping.Revision() || prepared.CommitID == "" || r.pending.CommitID != prepared.CommitID {
		return ErrBaselineInvalid
	}
	if len(r.pending.Resolutions) == 0 {
		return nil
	}
	return writeResolutionCommit(
		resolutionCommitPath(r.stateRoot),
		resolutionCommit{
			Format: "paperboat-config-resolution-commit-v1", CommitID: prepared.CommitID,
			Baseline: r.pending.Value, Resolutions: r.pending.Resolutions,
		},
	)
}

func (r *PlaintextWorkspaceReconciler) PublicationAborted(_ context.Context, prepared PreparedPublication) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	path := resolutionCommitPath(r.stateRoot)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	commit, err := readResolutionCommit(path)
	if err != nil || commit.CommitID != prepared.CommitID {
		return errors.Join(ErrBaselineInvalid, err)
	}
	return os.Remove(path)
}

func (r *PlaintextWorkspaceReconciler) recoverResolutionCommit(ctx context.Context, remoteRevision string) error {
	path := resolutionCommitPath(r.stateRoot)
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	commit, err := readResolutionCommit(path)
	if err != nil || commit.Baseline.RepositoryID != r.descriptor.RepositoryID ||
		commit.Baseline.AssignmentID != r.descriptor.AssignmentID ||
		commit.Baseline.RemoteRevision != commit.CommitID {
		return errors.Join(ErrBaselineInvalid, err)
	}
	if remoteRevision != commit.CommitID {
		return ErrSyncUncertain
	}
	if err := WriteBaseline(r.baselinePath, commit.Baseline); err != nil {
		return err
	}
	for _, resolution := range commit.Resolutions {
		if r.resolutions == nil {
			return ErrBaselineInvalid
		}
		if err := r.resolutions.Acknowledge(ctx, resolution.ID, commit.CommitID); err != nil {
			return err
		}
		_ = os.RemoveAll(filepath.Join(r.stateRoot, "conflicts", resolution.ConflictRevision))
	}
	return os.Remove(path)
}
