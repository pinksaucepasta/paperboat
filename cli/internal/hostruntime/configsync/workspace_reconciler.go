package configsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

var ErrWorkspaceReconcilerInvalid = errors.New("invalid config workspace reconciler")

type WorkspaceReconcilerConfig struct {
	HomeRoot              string
	StateRoot             string
	Descriptor            RuntimeDescriptor
	MachineSource         SourceConfig
	ApprovedConfiguration *RuntimeDescriptor
	SharedSource          func(context.Context) (SourceConfig, error)
	Resolutions           ConflictResolutionAuthority
	Clock                 func() time.Time
}

type pendingBaseline struct {
	CommitID    string
	Value       Baseline
	Resolutions []ConflictResolution
}

type ReconciliationDiagnostics struct {
	Skipped               []PathSummary
	Conflicts             []PathSummary
	ManifestHealth        string
	ManifestRevision      string
	ManagedPathCount      int
	PendingCleanPathCount int
	LastAppliedRevision   string
	LastPublishedRevision string
}

type DiagnosticsSource interface {
	Diagnostics() ReconciliationDiagnostics
}

type PlaintextWorkspaceReconciler struct {
	mapping               *PathMapping
	homeRoot              string
	stateRoot             string
	baselinePath          string
	descriptor            RuntimeDescriptor
	machineSource         SourceConfig
	approvedConfiguration RuntimeDescriptor
	sharedSource          func(context.Context) (SourceConfig, error)
	resolutions           ConflictResolutionAuthority
	clock                 func() time.Time

	mu          sync.Mutex
	pending     *pendingBaseline
	diagnostics ReconciliationDiagnostics
	manifest    Manifest
	observed    map[string]FileState
}

func NewPlaintextWorkspaceReconciler(config WorkspaceReconcilerConfig) (*PlaintextWorkspaceReconciler, error) {
	if !canonicalAbsolutePath(config.HomeRoot) || !canonicalAbsolutePath(config.StateRoot) ||
		validateRuntimeDescriptor(config.Descriptor, Credential{
			EnvironmentID: config.Descriptor.EnvironmentID, MachineID: config.Descriptor.MachineID,
			AssignmentID: config.Descriptor.AssignmentID, AssignmentVersion: config.Descriptor.AssignmentVersion, WarningRevision: config.Descriptor.WarningRevision,
		}) != nil {
		return nil, ErrWorkspaceReconcilerInvalid
	}
	for _, root := range []string{config.HomeRoot, config.StateRoot} {
		if err := checkSafeAbsolutePath(root); err != nil {
			return nil, errors.Join(ErrWorkspaceReconcilerInvalid, err)
		}
		info, err := os.Lstat(root)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.Join(ErrWorkspaceReconcilerInvalid, err)
		}
		resolved, err := filepath.EvalSymlinks(root)
		if err != nil || !mappedPlatformPathsEqual(resolved, root) {
			return nil, errors.Join(ErrWorkspaceReconcilerInvalid, err)
		}
	}
	config.Descriptor.Policy.AbsoluteRuntimeExclusionRoots = append(append([]string(nil), config.Descriptor.Policy.AbsoluteRuntimeExclusionRoots...), config.StateRoot)
	if config.Clock == nil {
		config.Clock = func() time.Time { return time.Now().UTC() }
	}
	approved := config.Descriptor
	if config.ApprovedConfiguration != nil {
		approved = *config.ApprovedConfiguration
	}
	reconciler := &PlaintextWorkspaceReconciler{
		approvedConfiguration: approved, sharedSource: config.SharedSource,
		homeRoot: config.HomeRoot, stateRoot: config.StateRoot,
		baselinePath: filepath.Join(config.StateRoot, "baseline.json"),
		descriptor:   config.Descriptor, machineSource: config.MachineSource, resolutions: config.Resolutions,
		clock: config.Clock,
	}
	return reconciler, nil
}

func (r *PlaintextWorkspaceReconciler) Reconcile(ctx context.Context, repositoryRoot string, remote RemoteSnapshot) (PreparedPublication, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.recoverResolutionCommit(ctx, remote.Revision); err != nil {
		return PreparedPublication{}, err
	}
	r.pending = nil
	r.diagnostics = ReconciliationDiagnostics{}
	if !canonicalAbsolutePath(repositoryRoot) || remote.Revision == "" {
		return PreparedPublication{}, ErrWorkspaceReconcilerInvalid
	}
	repository, err := git.PlainOpen(repositoryRoot)
	if err != nil {
		return PreparedPublication{}, ErrWorkspaceReconcilerInvalid
	}
	remoteHash := plumbing.NewHash(remote.Revision)
	if remoteHash.IsZero() {
		return PreparedPublication{}, ErrRemoteRevisionChanged
	}
	worktree, err := repository.Worktree()
	if err != nil {
		return PreparedPublication{}, err
	}
	if err := worktree.Reset(&git.ResetOptions{Commit: remoteHash, Mode: git.HardReset}); err != nil {
		return PreparedPublication{}, ErrRemoteRevisionChanged
	}
	if err := ValidateConfigRepository(repositoryRoot); err != nil {
		return PreparedPublication{}, err
	}
	var shared SourceConfig
	var sourceErr error
	if r.sharedSource != nil {
		shared, sourceErr = r.sharedSource(ctx)
	} else {
		shared, sourceErr = LoadSharedSourceConfig(repositoryRoot, DefaultSourceConfigLimits())
	}
	err = sourceErr
	if err != nil {
		r.diagnostics.ManifestHealth = "invalid"
		return PreparedPublication{}, err
	}
	effective, err := MergeSourceConfigs(shared, r.machineSource)
	if err != nil {
		return PreparedPublication{}, err
	}
	approved := r.approvedConfiguration
	if !effective.Enabled || effective.Revision != approved.ConfigurationRevision || ProjectionRevision(effective.Mode, effective.AutomaticUpdates, effective.PathRules) != ProjectionRevision(approved.Mode, approved.AutomaticUpdates, approved.PathRules) || !r.targetsMatch(effective) {
		return PreparedPublication{}, ErrConfigurationChanged
	}
	mapping, err := ResolveEffectiveConfig(r.homeRoot, effective)
	if err != nil {
		return PreparedPublication{}, err
	}
	r.mapping = mapping
	if err := recoverApplyJournal(filepath.Join(r.stateRoot, "apply-journal.json"), r.homeRoot, r.descriptor.RepositoryID, r.descriptor.AssignmentID, r.descriptor.Policy.MaxBatchBytes, mapping); err != nil {
		return PreparedPublication{}, err
	}
	manifest := effective.Manifest()
	r.manifest = manifest.Clone()
	r.diagnostics.ManifestRevision = manifest.Revision
	r.diagnostics.ManifestHealth = "healthy"
	if len(manifest.Roots) == 0 {
		r.diagnostics.ManifestHealth = "empty"
	}

	remoteHome := repositoryRoot
	remoteFiles, err := r.mapping.SnapshotContext(ctx, r.descriptor.Policy, manifest, repositoryRoot, false)
	if err != nil {
		return PreparedPublication{}, err
	}
	localFiles, err := r.mapping.SnapshotContext(ctx, r.descriptor.Policy, manifest, repositoryRoot, true)
	if err != nil {
		return PreparedPublication{}, err
	}
	r.diagnostics.Skipped = boundPathSummaries(
		append(append([]PathSummary(nil), localFiles.Skipped...), remoteFiles.Skipped...),
		r.descriptor.Policy.SummaryLimit,
	)
	managed := make(map[string]struct{}, len(localFiles.Files)+len(remoteFiles.Files))
	for path := range localFiles.Files {
		managed[path] = struct{}{}
	}
	for path := range remoteFiles.Files {
		managed[path] = struct{}{}
	}
	r.diagnostics.ManagedPathCount = len(managed)
	baselineFiles := map[string]FileState{}
	baselineRevision := ""
	baselineFrozen := map[string]FrozenPath{}
	if _, statErr := os.Lstat(r.baselinePath); errors.Is(statErr, os.ErrNotExist) {
		// A missing baseline is an explicit first reconciliation. The empty
		// ancestor makes differing local and remote values conflict.
	} else if statErr != nil {
		return PreparedPublication{}, statErr
	} else if baseline, baselineErr := ReadBaseline(r.baselinePath); baselineErr == nil {
		if baseline.RepositoryID != r.descriptor.RepositoryID || baseline.AssignmentID != r.descriptor.AssignmentID ||
			baseline.PolicyRevision != r.descriptor.Policy.Revision {
			return PreparedPublication{}, ErrBaselineInvalid
		}
		if baseline.MappingRevision == r.mapping.Revision() || len(baseline.PathDestinations) > 0 {
			baselineFiles = baseline.Files
			baselineRevision = baseline.RemoteRevision
			baselineFrozen = baseline.FrozenPaths
			if baseline.MappingRevision != r.mapping.Revision() {
				for name := range baselineFiles {
					target, ok := r.mapping.LocalPath(name)
					if !ok || baseline.PathDestinations[name] != target {
						delete(baselineFiles, name)
						delete(baselineFrozen, name)
					}
				}
			}
		}
		for name := range baselineFiles {
			target, ok := r.mapping.LocalPath(name)
			if !ok || !r.mapping.eligible(name, target, r.descriptor.Policy, manifest) {
				delete(baselineFiles, name)
				delete(baselineFrozen, name)
			}
		}
	} else {
		return PreparedPublication{}, baselineErr
	}
	effectiveMode := r.descriptor.Mode
	writesEnabled := r.descriptor.WriteMode == "leased_writes"
	if !writesEnabled && effectiveMode == ModeBidirectional {
		effectiveMode = ModePullOnly
	}
	plan := PlanReconciliationMode(baselineFiles, localFiles.Files, remoteFiles.Files, effectiveMode)
	if !writesEnabled {
		plan.PublishUpdates = nil
		plan.PublishDeletes = nil
	}
	mergedContents := map[string]mergedContent{}
	baseHome := ""
	mergeCandidates, frozenConflicts := partitionFrozenConflicts(plan.Conflicts, baselineFrozen)
	if len(mergeCandidates) > 0 && baselineRevision != "" {
		var cleanup func()
		var baseErr error
		baseHome, cleanup, baseErr = r.materializeRevision(ctx, repositoryRoot, worktree, remoteHash, baselineRevision)
		if baseErr != nil {
			return PreparedPublication{}, baseErr
		}
		defer cleanup()
	}
	if writesEnabled && effectiveMode == ModeBidirectional && len(mergeCandidates) > 0 && baseHome != "" {
		mergeCandidates, mergedContents, err = r.mergeConflicts(ctx, baseHome, remoteHome, baselineFiles, localFiles.Files, remoteFiles.Files, mergeCandidates)
		if err != nil {
			return PreparedPublication{}, err
		}
		for name := range mergedContents {
			plan.PublishUpdates = append(plan.PublishUpdates, name)
		}
		sort.Strings(plan.PublishUpdates)
		plan.PublishUpdates = uniqueSortedStrings(plan.PublishUpdates)
	}
	plan.Conflicts = append(frozenConflicts, mergeCandidates...)
	sort.Slice(plan.Conflicts, func(i, j int) bool { return plan.Conflicts[i].Path < plan.Conflicts[j].Path })
	if len(plan.Conflicts) > 0 || r.resolutions != nil {
		plan.Conflicts = identifyConflicts(
			r.descriptor.RepositoryID, r.descriptor.AssignmentID, remote.Revision,
			baselineFiles, localFiles.Files, remoteFiles.Files, plan.Conflicts,
		)
		resolved := make([]ConflictResolution, 0)
		forcedPaths := make(map[string]string)
		forcedMode := AssignmentMode("")
		if r.resolutions != nil {
			resolutions, resolutionErr := r.resolutions.Pending(ctx)
			if resolutionErr != nil {
				return PreparedPublication{}, resolutionErr
			}
			for _, resolution := range resolutions {
				if !resolution.Valid() || resolution.ExpectedRemoteRevision != remote.Revision {
					continue
				}
				if resolution.Scope == "config" {
					switch resolution.Action {
					case "force_pull":
						baselineFiles = cloneFileStates(localFiles.Files)
						forcedMode = ModePullOnly
					case "force_push":
						baselineFiles = cloneFileStates(remoteFiles.Files)
						forcedMode = ModePushOnly
					}
					resolved = append(resolved, resolution)
					continue
				}
				for _, conflict := range plan.Conflicts {
					if resolution.Path != conflict.Path || resolution.ConflictRevision != conflict.Revision {
						continue
					}
					switch resolution.Action {
					case "keep_local", "force_push":
						setBaselineState(baselineFiles, resolution.Path, remoteFiles.Files)
					case "keep_remote", "force_pull":
						setBaselineState(baselineFiles, resolution.Path, localFiles.Files)
					}
					if resolution.Action == "force_pull" || resolution.Action == "force_push" {
						forcedPaths[resolution.Path] = resolution.Action
					}
					resolved = append(resolved, resolution)
					delete(baselineFrozen, resolution.Path)
					break
				}
			}
			if len(resolved) > 0 {
				planningMode := effectiveMode
				if forcedMode.Valid() {
					planningMode = forcedMode
				}
				plan = PlanReconciliationMode(baselineFiles, localFiles.Files, remoteFiles.Files, planningMode)
				for path, action := range forcedPaths {
					forcePlanPath(&plan, path, action, localFiles.Files, remoteFiles.Files)
				}
				if !writesEnabled {
					plan.PublishUpdates = nil
					plan.PublishDeletes = nil
				}
				plan.Conflicts = identifyConflicts(
					r.descriptor.RepositoryID, r.descriptor.AssignmentID, remote.Revision,
					baselineFiles, localFiles.Files, remoteFiles.Files, plan.Conflicts,
				)
			}
		}
		r.diagnostics.Conflicts = boundPathSummaries(plan.Conflicts, r.descriptor.Policy.SummaryLimit)
		if len(plan.Conflicts) > 0 {
			newConflicts := make([]PathSummary, 0, len(plan.Conflicts))
			for _, conflict := range plan.Conflicts {
				if _, frozen := baselineFrozen[conflict.Path]; !frozen {
					newConflicts = append(newConflicts, conflict)
				}
			}
			if err := r.preserveConflicts(
				baselineFiles, localFiles.Files, remoteFiles.Files, newConflicts,
				baseHome, remoteHome, baselineRevision, remote.Revision,
			); err != nil {
				return PreparedPublication{}, err
			}
		}
		if len(resolved) > 0 {
			defer func() {
				if r.pending != nil {
					r.pending.Resolutions = append(r.pending.Resolutions, resolved...)
				}
			}()
		}
	}
	r.diagnostics.PendingCleanPathCount = len(plan.PublishUpdates) + len(plan.PublishDeletes) + len(plan.ApplyRemote) + len(plan.DeleteLocal)

	applyPaths := append(append([]string(nil), plan.ApplyRemote...), plan.DeleteLocal...)
	for name := range mergedContents {
		applyPaths = append(applyPaths, name)
	}
	journalPath := filepath.Join(r.stateRoot, "apply-journal.json")
	if len(applyPaths) > 0 {
		if err := beginApplyJournal(
			journalPath, r.homeRoot, r.descriptor.RepositoryID, r.descriptor.AssignmentID,
			remote.Revision, applyPaths, r.descriptor.Policy.MaxBatchBytes, r.mapping,
		); err != nil {
			return PreparedPublication{}, err
		}
	}
	applySucceeded := false
	defer func() {
		if len(applyPaths) > 0 && !applySucceeded {
			_ = recoverApplyJournal(
				journalPath, r.homeRoot, r.descriptor.RepositoryID, r.descriptor.AssignmentID,
				r.descriptor.Policy.MaxBatchBytes, r.mapping,
			)
		}
	}()
	for _, name := range plan.ApplyRemote {
		value, err := readVerifiedState(repositoryRoot, name, remoteFiles.Files[name], r.descriptor.Policy.MaxFileBytes)
		if err != nil {
			return PreparedPublication{}, err
		}
		if err := r.mapping.write(name, value, remoteFiles.Files[name].Mode); err != nil {
			return PreparedPublication{}, err
		}
	}
	for _, path := range plan.DeleteLocal {
		if err := r.mapping.remove(path); err != nil {
			return PreparedPublication{}, err
		}
	}
	for name, merged := range mergedContents {
		if err := r.mapping.write(name, merged.Value, merged.Mode); err != nil {
			return PreparedPublication{}, err
		}
	}
	if len(applyPaths) > 0 {
		// Retain exactly one bounded, private pre-apply image for deliberate
		// rollback. A later successful apply atomically replaces it.
		if err := os.Rename(journalPath, filepath.Join(r.stateRoot, "rollback.json")); err != nil {
			return PreparedPublication{}, err
		}
	}
	applySucceeded = true
	if len(applyPaths) > 0 {
		r.diagnostics.LastAppliedRevision = remote.Revision
	}
	for _, name := range plan.PublishUpdates {
		state, ok := localFiles.Files[name]
		if !ok {
			state = FileState{Hash: hashSnapshotBytes(mergedContents[name].Value), Bytes: int64(len(mergedContents[name].Value)), Mode: mergedContents[name].Mode}
		}
		if merged, ok := mergedContents[name]; ok {
			state = FileState{Hash: hashSnapshotBytes(merged.Value), Bytes: int64(len(merged.Value)), Mode: merged.Mode}
		}
		value, err := r.mapping.read(name, state, r.descriptor.Policy.MaxFileBytes)
		if err != nil {
			return PreparedPublication{}, err
		}
		if err := writeMergedTarget(repositoryRoot, name, mergedContent{value, state.Mode}); err != nil {
			return PreparedPublication{}, err
		}
	}
	for _, name := range plan.PublishDeletes {
		if err := removeManagedPath(repositoryRoot, name); err != nil {
			return PreparedPublication{}, err
		}
	}
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		return PreparedPublication{}, err
	}
	status, err := worktree.Status()
	if err != nil {
		return PreparedPublication{}, err
	}
	if status.IsClean() {
		merged, snapshotErr := r.mapping.SnapshotContext(ctx, r.descriptor.Policy, manifest, repositoryRoot, true)
		if snapshotErr != nil {
			return PreparedPublication{}, snapshotErr
		}
		r.observeLocal(merged.Files, plan.DeleteLocal)
		accepted := AcceptedBaseline(baselineFiles, merged.Files, remoteFiles.Files, r.descriptor.Mode, writesEnabled)
		freezeConflictBaseline(accepted, baselineFiles, plan.Conflicts)
		frozen := nextFrozenPaths(baselineFrozen, plan.Conflicts, baselineRevision)
		r.pending = &pendingBaseline{CommitID: remote.Revision, Value: Baseline{
			Format: "paperboat-config-baseline-v1", RepositoryID: r.descriptor.RepositoryID,
			AssignmentID: r.descriptor.AssignmentID, PolicyRevision: r.descriptor.Policy.Revision,
			MappingRevision: r.mapping.Revision(), PathDestinations: r.mapping.destinations(accepted), ManifestRevision: manifest.Revision, SelectedRoots: append([]ManifestRoot{}, manifest.Roots...),
			FrozenPaths: frozen, RemoteRevision: remote.Revision, Files: accepted,
		}}
		r.diagnostics.PendingCleanPathCount = 0
		return PreparedPublication{ExpectedRemoteRevision: remote.Revision, CommitID: remote.Revision, HasChanges: false}, nil
	}
	if err := validateRepositoryBatch(repositoryRoot, status, r.descriptor.Policy); err != nil {
		return PreparedPublication{}, err
	}
	commit, err := worktree.Commit("Synchronize configuration", &git.CommitOptions{
		Author: &object.Signature{Name: "Paperboat", Email: "config@paperboat.invalid", When: r.clock()},
	})
	if err != nil {
		return PreparedPublication{}, err
	}
	merged, err := r.mapping.SnapshotContext(ctx, r.descriptor.Policy, manifest, repositoryRoot, true)
	if err != nil {
		return PreparedPublication{}, err
	}
	r.observeLocal(merged.Files, plan.DeleteLocal)
	accepted := AcceptedBaseline(baselineFiles, merged.Files, merged.Files, r.descriptor.Mode, writesEnabled)
	freezeConflictBaseline(accepted, baselineFiles, plan.Conflicts)
	frozen := nextFrozenPaths(baselineFrozen, plan.Conflicts, baselineRevision)
	r.pending = &pendingBaseline{CommitID: commit.String(), Value: Baseline{
		Format: "paperboat-config-baseline-v1", RepositoryID: r.descriptor.RepositoryID,
		AssignmentID: r.descriptor.AssignmentID, PolicyRevision: r.descriptor.Policy.Revision,
		MappingRevision: r.mapping.Revision(), PathDestinations: r.mapping.destinations(accepted), ManifestRevision: manifest.Revision, SelectedRoots: append([]ManifestRoot{}, manifest.Roots...),
		FrozenPaths: frozen, RemoteRevision: commit.String(), Files: accepted,
	}}
	r.diagnostics.PendingCleanPathCount = 0
	r.diagnostics.LastPublishedRevision = commit.String()
	return PreparedPublication{
		ExpectedRemoteRevision: remote.Revision, CommitID: commit.String(), HasChanges: true,
	}, nil
}

// Rollback restores the last successfully displaced managed paths. It clears
// the reconciliation baseline so the next sync must be reviewed/reconciled
// against the restored local state instead of silently accepting stale state.
func (r *PlaintextWorkspaceReconciler) Rollback() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	err := recoverApplyJournal(filepath.Join(r.stateRoot, "rollback.json"), r.homeRoot, r.descriptor.RepositoryID, r.descriptor.AssignmentID, r.descriptor.Policy.MaxBatchBytes, r.mapping)
	if err != nil {
		return err
	}
	if removeErr := os.Remove(r.baselinePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return removeErr
	}
	r.pending = nil
	return nil
}

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

func (r *PlaintextWorkspaceReconciler) preserveConflicts(
	baseline, local, remote map[string]FileState,
	conflicts []PathSummary,
	baseHome, remoteHome, baseRevision, remoteRevision string,
) error {
	for _, conflict := range conflicts {
		conflictRoot := filepath.Join(r.stateRoot, "conflicts", conflict.Revision)
		if err := os.MkdirAll(conflictRoot, 0o700); err != nil {
			return err
		}
		for _, side := range []struct {
			name  string
			root  string
			state FileState
			ok    bool
		}{
			{"base", baseHome, baseline[conflict.Path], hasState(baseline, conflict.Path)},
			{"local", r.homeRoot, local[conflict.Path], hasState(local, conflict.Path)},
			{"remote", remoteHome, remote[conflict.Path], hasState(remote, conflict.Path)},
		} {
			target := filepath.Join(conflictRoot, filepath.FromSlash(conflict.Path)+"."+side.name)
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return err
			}
			if info, statErr := os.Lstat(target); statErr == nil {
				if !privateControlFile(target, info) {
					return ErrConfigRepositoryInvalid
				}
				continue
			} else if !errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
			if !side.ok {
				if err := writePrivateAtomic(target, []byte(`{"deleted":true}`+"\n")); err != nil {
					return err
				}
				continue
			}
			if side.root == "" {
				return ErrBaselineInvalid
			}
			if side.state.Mode&os.ModeSymlink != 0 {
				if err := writePrivateAtomic(target, []byte(side.state.Target)); err != nil {
					return err
				}
				continue
			}
			source := filepath.Join(side.root, filepath.FromSlash(conflict.Path))
			if side.name == "local" {
				var ok bool
				source, ok = r.mapping.LocalPath(conflict.Path)
				if !ok {
					return ErrPathRuleInvalid
				}
			}
			if err := writeConflictFile(target, source, side.state); err != nil {
				return err
			}
		}
		metadata, err := json.Marshal(struct {
			RepositoryID   string      `json:"repository_id"`
			AssignmentID   string      `json:"assignment_id"`
			BaseRevision   string      `json:"base_revision,omitempty"`
			RemoteRevision string      `json:"remote_revision"`
			BaseDigest     string      `json:"base_digest"`
			LocalDigest    string      `json:"local_digest"`
			RemoteDigest   string      `json:"remote_digest"`
			Conflict       PathSummary `json:"conflict"`
		}{
			r.descriptor.RepositoryID, r.descriptor.AssignmentID, baseRevision, remoteRevision,
			stateIdentity(baseline, conflict.Path), stateIdentity(local, conflict.Path),
			stateIdentity(remote, conflict.Path), conflict,
		})
		if err != nil {
			return err
		}
		if err := writePrivateAtomic(filepath.Join(conflictRoot, "metadata.json"), append(metadata, '\n')); err != nil {
			return err
		}
	}
	return nil
}

func hasState(states map[string]FileState, path string) bool {
	_, ok := states[path]
	return ok
}

func writeConflictFile(target, source string, expected FileState) error {
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Size() != expected.Bytes {
		return errors.Join(ErrSourceChanged, err)
	}
	value, opened, err := secureReadFile(source, expected.Bytes)
	if err == nil && (!os.SameFile(info, opened) || hashSnapshotBytes(value) != expected.Hash) {
		return ErrSourceChanged
	}
	if err != nil {
		return err
	}
	return writePrivateAtomic(target, value)
}

func removeManagedPath(root, relative string) error {
	if !safeRelativeStatusPath(relative) {
		return ErrWorkspaceReconcilerInvalid
	}
	target := filepath.Join(root, filepath.FromSlash(relative))
	if !sameOrInsidePath(target, root) {
		return ErrWorkspaceReconcilerInvalid
	}
	return secureRemoveFile(target)
}

func validateRepositoryBatch(root string, status git.Status, policy RuntimePolicy) error {
	var total int64
	for relative, fileStatus := range status {
		if relative == ".git" || strings.HasPrefix(relative, ".git/") ||
			fileStatus.Worktree == git.Deleted || fileStatus.Staging == git.Deleted {
			continue
		}
		path := filepath.Join(root, filepath.FromSlash(relative))
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return errors.Join(ErrConfigRepositoryInvalid, err)
		}
		if info.Size() > policy.MaxFileBytes+(64<<10) {
			return ErrConfigRepositoryInvalid
		}
		total += info.Size()
		if total > policy.MaxBatchBytes+(1<<20) {
			return ErrConfigRepositoryInvalid
		}
	}
	return nil
}

func boundPathSummaries(values []PathSummary, limit int) []PathSummary {
	sort.Slice(values, func(i, j int) bool {
		if values[i].Path == values[j].Path {
			return values[i].Reason < values[j].Reason
		}
		return values[i].Path < values[j].Path
	})
	result := values[:0]
	for _, value := range values {
		if len(result) > 0 && result[len(result)-1].Path == value.Path && result[len(result)-1].Reason == value.Reason {
			continue
		}
		result = append(result, value)
		if len(result) == limit {
			break
		}
	}
	return result
}

func (r *PlaintextWorkspaceReconciler) observeLocal(files map[string]FileState, deleted []string) {
	r.observed = cloneFileStates(files)
	for _, name := range deleted {
		r.observed[name] = FileState{}
	}
}

// ObservedLocalFile lets watches distinguish already-reconciled output from
// subsequent user changes without ignoring edits that happen during sync.
func (r *PlaintextWorkspaceReconciler) ObservedLocalFile(name string) (FileState, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	state, ok := r.observed[name]
	return state, ok
}

func (r *PlaintextWorkspaceReconciler) targetsMatch(config EffectiveConfig) bool {
	for direction, entry := range []struct {
		target *RepositoryTarget
		id     string
	}{{config.Pull, r.approvedConfiguration.PullRepositoryID}, {config.Push, r.approvedConfiguration.PushRepositoryID}} {
		if direction == 0 && config.Mode == ModePushOnly || direction == 1 && config.Mode == ModePullOnly {
			continue
		}
		if entry.target == nil {
			continue
		}
		id := entry.id
		if id == "" {
			id = r.approvedConfiguration.RepositoryID
		}
		if entry.target.RepositoryID != id {
			return false
		}
		if entry.target.URL != "" {
			matched := false
			for _, binding := range r.approvedConfiguration.RepositoryBindings {
				if binding.RepositoryID == id && entry.target.URL == binding.URL {
					matched = true
				}
			}
			if !matched {
				return false
			}
		}
	}
	return true
}
