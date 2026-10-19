package configsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
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
	ComparisonStatusPath  string
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
	comparisonStatusPath  string
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
		(config.ComparisonStatusPath != "" && !canonicalAbsolutePath(config.ComparisonStatusPath)) ||
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
		homeRoot: config.HomeRoot, stateRoot: config.StateRoot, comparisonStatusPath: config.ComparisonStatusPath,
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
	} else if baseline, baselineErr := ReadBaseline(r.baselinePath); baselineErr != nil {
		return PreparedPublication{}, baselineErr
	} else if baseline.RepositoryID == r.descriptor.RepositoryID && baseline.AssignmentID == r.descriptor.AssignmentID {
		// A baseline recorded for another repository or assignment is no ancestor of this one, so it is
		// ignored like a missing baseline (differing files conflict). A newer policy keeps the ancestor:
		// files the policy no longer allows are dropped below and the baseline is rewritten with the new
		// policy revision when the sync commits.
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
