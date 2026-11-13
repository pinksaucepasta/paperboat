package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// partitionFrozenConflicts splits the conflicts into those to merge now and those still frozen. A conflict frozen by an
// earlier round stays frozen only while it is the same disagreement (same local and remote content): once either side
// moved, it is merged again, with the ancestor it was frozen at (returned per path) because the baseline moved on.
func partitionFrozenConflicts(conflicts []PathSummary, frozen map[string]FrozenPath) ([]PathSummary, []PathSummary, map[string]string) {
	mergeCandidates := make([]PathSummary, 0, len(conflicts))
	frozenConflicts := make([]PathSummary, 0, len(conflicts))
	ancestors := map[string]string{}
	for _, conflict := range conflicts {
		ancestry, ok := frozen[conflict.Path]
		switch {
		case !ok:
			mergeCandidates = append(mergeCandidates, conflict)
		case ancestry.ConflictRevision != conflict.Revision && ancestry.BaseRevision != "absent":
			ancestors[conflict.Path] = ancestry.BaseRevision
			mergeCandidates = append(mergeCandidates, conflict)
		default:
			conflict.Revision = ancestry.ConflictRevision
			frozenConflicts = append(frozenConflicts, conflict)
		}
	}
	return mergeCandidates, frozenConflicts, ancestors
}

func nextFrozenPaths(previous map[string]FrozenPath, conflicts []PathSummary, baseRevision string) map[string]FrozenPath {
	if baseRevision == "" {
		baseRevision = "absent"
	}
	result := make(map[string]FrozenPath, len(conflicts))
	for _, conflict := range conflicts {
		base := baseRevision
		if frozen, ok := previous[conflict.Path]; ok {
			base = frozen.BaseRevision // the ancestor stays the one the disagreement started from
		}
		result[conflict.Path] = FrozenPath{BaseRevision: base, ConflictRevision: conflict.Revision}
	}
	return result
}

// mergeRun is what mergeByAncestor needs from one reconciliation round.
type mergeRun struct {
	repositoryRoot   string
	worktree         *git.Worktree
	remoteHash       plumbing.Hash
	remoteHome       string
	baselineRevision string
	mode             AssignmentMode
	writesEnabled    bool
	baseline         map[string]FileState
	local, remote    map[string]FileState
	ancestors        map[string]string // ancestor revision of conflicts released from a freeze
	baseHomes        map[string]string // ancestor checkout of each conflict, for the saved conflict sides
	cleanups         []func()
}

func (m *mergeRun) cleanup() {
	for _, cleanup := range m.cleanups {
		cleanup()
	}
}

// mergeByAncestor merges (push half) or adopts an already merged remote (pull half) for each candidate, with the
// ancestor that belongs to it: the baseline revision, or the revision a released conflict was frozen at. It returns the
// candidates that stay conflicts and adds merged paths to the plan.
func (r *PlaintextWorkspaceReconciler) mergeByAncestor(
	ctx context.Context, run *mergeRun, candidates []PathSummary, plan *ReconcilePlan, mergedContents map[string]mergedContent,
) ([]PathSummary, error) {
	groups := map[string][]PathSummary{}
	for _, conflict := range candidates {
		revision := run.baselineRevision
		if ancestor, released := run.ancestors[conflict.Path]; released {
			revision = ancestor
		}
		groups[revision] = append(groups[revision], conflict)
	}
	revisions := make([]string, 0, len(groups))
	for revision := range groups {
		revisions = append(revisions, revision)
	}
	sort.Strings(revisions)
	var remaining []PathSummary
	for _, revision := range revisions {
		left, err := r.mergeGroup(ctx, run, revision, groups[revision], plan, mergedContents)
		if err != nil {
			return nil, err
		}
		remaining = append(remaining, left...)
	}
	plan.PublishUpdates = uniqueSortedStrings(sortedCopy(plan.PublishUpdates))
	plan.ApplyRemote = uniqueSortedStrings(sortedCopy(plan.ApplyRemote))
	return remaining, nil
}

func sortedCopy(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}

func (r *PlaintextWorkspaceReconciler) mergeGroup(
	ctx context.Context, run *mergeRun, revision string, group []PathSummary, plan *ReconcilePlan, mergedContents map[string]mergedContent,
) ([]PathSummary, error) {
	if revision == "" {
		return group, nil
	}
	baseHome, cleanup, err := r.materializeRevision(ctx, run.repositoryRoot, run.worktree, run.remoteHash, revision)
	if err != nil {
		for _, conflict := range group {
			if _, released := run.ancestors[conflict.Path]; !released {
				return nil, err
			}
		}
		return group, nil // a released conflict whose ancestor is gone simply stays a conflict
	}
	run.cleanups = append(run.cleanups, cleanup)
	for _, conflict := range group {
		run.baseHomes[conflict.Path] = baseHome
	}
	if run.writesEnabled && (run.mode == ModeBidirectional || r.mergeConcurrentEdits) {
		var merged map[string]mergedContent
		group, merged, err = r.mergeConflicts(ctx, baseHome, run.remoteHome, run.baseline, run.local, run.remote, group)
		if err != nil {
			return nil, err
		}
		for name, value := range merged {
			mergedContents[name] = value
			plan.PublishUpdates = append(plan.PublishUpdates, name)
		}
	}
	if run.mode == ModePullOnly && !r.mergeConcurrentEdits {
		var adopted []string
		group, adopted, err = r.mergesEqualToRemote(ctx, baseHome, run.remoteHome, run.baseline, run.local, run.remote, group)
		if err != nil {
			return nil, err
		}
		plan.ApplyRemote = append(plan.ApplyRemote, adopted...)
	}
	return group, nil
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

// mergesEqualToRemote is the pull half's share of a merge. It cannot publish one, but when the other machine already
// merged both edits (its push half published them) the three-way merge equals the remote file, so the remote is simply
// applied; any other merge stays a conflict for the push half to merge and publish.
func (r *PlaintextWorkspaceReconciler) mergesEqualToRemote(
	ctx context.Context,
	baseHome, remoteHome string,
	baseline, local, remote map[string]FileState,
	conflicts []PathSummary,
) ([]PathSummary, []string, error) {
	remaining, merged, err := r.mergeConflicts(ctx, baseHome, remoteHome, baseline, local, remote, conflicts)
	if err != nil {
		return nil, nil, err
	}
	var adopted []string
	for _, conflict := range conflicts {
		value, ok := merged[conflict.Path]
		if !ok {
			continue
		}
		if hashSnapshotBytes(value.Value) == remote[conflict.Path].Hash && value.Mode == remote[conflict.Path].Mode.Perm() {
			adopted = append(adopted, conflict.Path)
		} else {
			remaining = append(remaining, conflict)
		}
	}
	return remaining, adopted, nil
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
