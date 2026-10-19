package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func partitionFrozenConflicts(conflicts []PathSummary, frozen map[string]FrozenPath) ([]PathSummary, []PathSummary) {
	mergeCandidates := make([]PathSummary, 0, len(conflicts))
	frozenConflicts := make([]PathSummary, 0, len(conflicts))
	for _, conflict := range conflicts {
		if ancestry, ok := frozen[conflict.Path]; ok {
			conflict.Revision = ancestry.ConflictRevision
			frozenConflicts = append(frozenConflicts, conflict)
		} else {
			mergeCandidates = append(mergeCandidates, conflict)
		}
	}
	return mergeCandidates, frozenConflicts
}

func nextFrozenPaths(previous map[string]FrozenPath, conflicts []PathSummary, baseRevision string) map[string]FrozenPath {
	if baseRevision == "" {
		baseRevision = "absent"
	}
	result := make(map[string]FrozenPath, len(conflicts))
	for _, conflict := range conflicts {
		if frozen, ok := previous[conflict.Path]; ok {
			result[conflict.Path] = frozen
		} else {
			result[conflict.Path] = FrozenPath{BaseRevision: baseRevision, ConflictRevision: conflict.Revision}
		}
	}
	return result
}

func forcePlanPath(plan *ReconcilePlan, path, action string, local, remote map[string]FileState) {
	plan.PublishUpdates = removeString(plan.PublishUpdates, path)
	plan.PublishDeletes = removeString(plan.PublishDeletes, path)
	plan.ApplyRemote = removeString(plan.ApplyRemote, path)
	plan.DeleteLocal = removeString(plan.DeleteLocal, path)
	conflicts := plan.Conflicts[:0]
	for _, conflict := range plan.Conflicts {
		if conflict.Path != path {
			conflicts = append(conflicts, conflict)
		}
	}
	plan.Conflicts = conflicts
	if action == "force_pull" {
		if _, ok := remote[path]; ok {
			plan.ApplyRemote = append(plan.ApplyRemote, path)
		} else {
			plan.DeleteLocal = append(plan.DeleteLocal, path)
		}
		return
	}
	if _, ok := local[path]; ok {
		plan.PublishUpdates = append(plan.PublishUpdates, path)
	} else {
		plan.PublishDeletes = append(plan.PublishDeletes, path)
	}
}

func removeString(values []string, target string) []string {
	result := values[:0]
	for _, value := range values {
		if value != target {
			result = append(result, value)
		}
	}
	return result
}

type mergedContent struct {
	Value []byte
	Mode  os.FileMode
}

func (r *PlaintextWorkspaceReconciler) materializeRevision(
	ctx context.Context,
	repositoryRoot string,
	worktree *git.Worktree,
	remoteHash plumbing.Hash,
	revision string,
) (string, func(), error) {
	baseHash := plumbing.NewHash(revision)
	if baseHash.IsZero() {
		return "", func() {}, ErrBaselineInvalid
	}
	if err := worktree.Reset(&git.ResetOptions{Commit: baseHash, Mode: git.HardReset}); err != nil {
		return "", func() {}, ErrBaselineInvalid
	}
	restored := false
	restore := func() error {
		if restored {
			return nil
		}
		restored = true
		return worktree.Reset(&git.ResetOptions{Commit: remoteHash, Mode: git.HardReset})
	}
	baseHome, err := os.MkdirTemp(r.stateRoot, "base-home-")
	if err != nil {
		_ = restore()
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(baseHome) }
	snapshot, err := r.mapping.SnapshotContext(ctx, r.descriptor.Policy, r.manifest, repositoryRoot, false)
	if err == nil {
		for name, state := range snapshot.Files {
			if err = ctx.Err(); err != nil {
				break
			}
			var value []byte
			value, err = readVerifiedState(repositoryRoot, name, state, r.descriptor.Policy.MaxFileBytes)
			if err != nil {
				break
			}
			err = writeMergedTarget(baseHome, name, mergedContent{value, state.Mode})
			if err != nil {
				break
			}
		}
	}
	restoreErr := restore()
	if err != nil || restoreErr != nil {
		cleanup()
		return "", func() {}, errors.Join(err, restoreErr)
	}
	return baseHome, cleanup, nil
}

func (r *PlaintextWorkspaceReconciler) mergeConflicts(
	ctx context.Context,
	baseHome, remoteHome string,
	baseline, local, remote map[string]FileState,
	conflicts []PathSummary,
) ([]PathSummary, map[string]mergedContent, error) {
	remaining := make([]PathSummary, 0, len(conflicts))
	merged := make(map[string]mergedContent)
	for _, conflict := range conflicts {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		baseState, baseOK := baseline[conflict.Path]
		localState, localOK := local[conflict.Path]
		remoteState, remoteOK := remote[conflict.Path]
		if !baseOK || !localOK || !remoteOK {
			conflict.Reason = "delete_modify"
			remaining = append(remaining, conflict)
			continue
		}
		if baseState.Mode&os.ModeSymlink != 0 || localState.Mode&os.ModeSymlink != 0 || remoteState.Mode&os.ModeSymlink != 0 {
			conflict.Reason = "type_change"
			remaining = append(remaining, conflict)
			continue
		}
		if baseState.Mode.Perm() != localState.Mode.Perm() || localState.Mode.Perm() != remoteState.Mode.Perm() {
			conflict.Reason = "mode_change"
			remaining = append(remaining, conflict)
			continue
		}
		baseValue, baseErr := readVerifiedState(baseHome, conflict.Path, baseState, r.descriptor.Policy.MaxFileBytes)
		localValue, localErr := r.mapping.read(conflict.Path, localState, r.descriptor.Policy.MaxFileBytes)
		remoteValue, remoteErr := readVerifiedState(remoteHome, conflict.Path, remoteState, r.descriptor.Policy.MaxFileBytes)
		if baseErr != nil || localErr != nil || remoteErr != nil {
			conflict.Reason = "source_changed"
			remaining = append(remaining, conflict)
			continue
		}
		value, mergeErr := mergeRegularText(ctx, baseValue, localValue, remoteValue, r.descriptor.Policy.MaxFileBytes)
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if mergeErr != nil {
			conflict.Reason = "merge_conflict"
			remaining = append(remaining, conflict)
			continue
		}
		merged[conflict.Path] = mergedContent{Value: value, Mode: localState.Mode.Perm()}
	}
	return remaining, merged, nil
}

func readVerifiedState(root, relative string, expected FileState, maxBytes int64) ([]byte, error) {
	if !safeRelativeStatusPath(relative) || expected.Bytes < 0 || expected.Bytes > maxBytes {
		return nil, ErrSourceChanged
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() != expected.Bytes || (canonicalMappedMode(info) != expected.Mode.Perm() && snapshotFileMode(info) != expected.Mode.Perm()) {
		return nil, errors.Join(ErrSourceChanged, err)
	}
	value, opened, err := secureReadFile(path, maxBytes)
	if err == nil && !os.SameFile(info, opened) {
		return nil, ErrSourceChanged
	}
	if err != nil {
		return nil, err
	}
	hash := sha256.Sum256(value)
	if hex.EncodeToString(hash[:]) != expected.Hash {
		return nil, ErrSourceChanged
	}
	return value, nil
}

func writeMergedTarget(root, relative string, merged mergedContent) error {
	if !safeRelativeStatusPath(relative) || merged.Mode&^os.ModePerm != 0 {
		return ErrWorkspaceReconcilerInvalid
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	return secureWriteFile(target, merged.Value, merged.Mode)
}

func uniqueSortedStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	result := values[:1]
	for _, value := range values[1:] {
		if value != result[len(result)-1] {
			result = append(result, value)
		}
	}
	return result
}
