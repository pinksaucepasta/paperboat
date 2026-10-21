package configsync

import (
	"context"
	"errors"

	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

type testResolutionAuthority struct {
	items        []ConflictResolution
	acknowledged []string
}

func (a *testResolutionAuthority) Pending(context.Context) ([]ConflictResolution, error) {
	return append([]ConflictResolution(nil), a.items...), nil
}

func (a *testResolutionAuthority) Acknowledge(_ context.Context, id, _ string) error {
	a.acknowledged = append(a.acknowledged, id)
	return nil
}

func TestWorkspaceReconcilerPublishesCleanMergeFromPersistedBase(t *testing.T) {
	// The production reconciler must merge, commit and preserve conflicts without Git.
	t.Setenv("PATH", t.TempDir())
	root := resolvedTempDir(t)
	repositoryRoot := filepath.Join(root, "repository")
	homeRoot := filepath.Join(root, "home")
	configRoot := filepath.Join(root, "selected-config")
	assignmentRoot := filepath.Join(root, "state")
	stateRoot := filepath.Join(assignmentRoot, "pull")
	for _, path := range []string{repositoryRoot, homeRoot, stateRoot, configRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, filepath.Join(repositoryRoot, "config.txt"), "one\ntwo\nthree\n")
	writeTestFile(t, filepath.Join(repositoryRoot, "clean.txt"), "clean base\n")
	if err := os.Chmod(filepath.Join(repositoryRoot, "config.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(repositoryRoot, "clean.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(configRoot, "config.txt"), "one\ntwo\nthree\n")
	writeTestFile(t, filepath.Join(configRoot, "clean.txt"), "clean base\n")
	if err := os.Chmod(filepath.Join(configRoot, "config.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(configRoot, "clean.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	repository, err := git.PlainInit(repositoryRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	baseRevision := commitAll(t, repository, "base")

	descriptor := testRuntimeDescriptor()
	descriptor.PathRules = []PathRule{{ID: "config", RepositoryPath: "config.txt", LocalPath: filepath.Join(configRoot, "config.txt"), Kind: "file"}, {ID: "clean", RepositoryPath: "clean.txt", LocalPath: filepath.Join(configRoot, "clean.txt"), Kind: "file"}}
	reconciler, err := newTestFileReconciler(t, WorkspaceReconcilerConfig{
		HomeRoot: homeRoot, StateRoot: stateRoot, Descriptor: descriptor,
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := reconciler.Reconcile(context.Background(), repositoryRoot, RemoteSnapshot{Revision: baseRevision})
	if err != nil || prepared.HasChanges || len(reconciler.Diagnostics().Conflicts) > 0 {
		t.Fatalf("initial reconcile = %#v, diagnostics = %#v, %v", prepared, reconciler.Diagnostics(), err)
	}
	if err := reconciler.PublicationCommitted(context.Background(), prepared, baseRevision); err != nil {
		t.Fatal(err)
	}
	initialBaseline, err := ReadBaseline(filepath.Join(stateRoot, "baseline.json"))
	if err != nil || initialBaseline.Files["config.txt"].Hash == "" {
		t.Fatalf("initial baseline = %#v, %v", initialBaseline, err)
	}

	writeTestFile(t, filepath.Join(configRoot, "config.txt"), "ONE\ntwo\nthree\n")
	if err := os.Chmod(filepath.Join(configRoot, "config.txt"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repositoryRoot, "config.txt"), "one\ntwo\nTHREE\n")
	remoteRevision := commitAll(t, repository, "remote change")
	prepared, err = reconciler.Reconcile(context.Background(), repositoryRoot, RemoteSnapshot{Revision: remoteRevision})
	if err != nil || !prepared.HasChanges || prepared.ExpectedRemoteRevision != remoteRevision {
		t.Fatalf("merged reconcile = %#v, diagnostics = %#v, %v", prepared, reconciler.Diagnostics(), err)
	}
	mergedCommit, commitErr := repository.CommitObject(plumbing.NewHash(prepared.CommitID))
	if commitErr != nil || mergedCommit.Author.Name != "Paperboat" || mergedCommit.Author.Email != "config@paperboat.invalid" {
		t.Fatalf("explicit sync commit identity missing: %v", commitErr)
	}
	value, err := os.ReadFile(filepath.Join(configRoot, "config.txt"))
	if err != nil || string(value) != "ONE\ntwo\nTHREE\n" {
		t.Fatalf("merged target = %q, %v", value, err)
	}
	if conflicts := reconciler.Diagnostics().Conflicts; len(conflicts) != 0 {
		t.Fatalf("clean merge conflicts = %#v", conflicts)
	}
	if err := reconciler.PublicationCommitted(context.Background(), prepared, prepared.CommitID); err != nil {
		t.Fatal(err)
	}
	baseline, err := ReadBaseline(filepath.Join(stateRoot, "baseline.json"))
	if err != nil || baseline.RemoteRevision != prepared.CommitID || baseline.ManifestRevision == "" || len(baseline.SelectedRoots) != 2 {
		t.Fatalf("merged baseline = %#v, %v", baseline, err)
	}
	mergedBaseState := baseline.Files["config.txt"]

	writeTestFile(t, filepath.Join(configRoot, "config.txt"), "LOCAL\ntwo\nTHREE\n")
	writeTestFile(t, filepath.Join(configRoot, "clean.txt"), "clean local\n")
	writeTestFile(t, filepath.Join(repositoryRoot, "config.txt"), "REMOTE\ntwo\nTHREE\n")
	conflictingRemote := commitAll(t, repository, "overlapping remote change")
	prepared, err = reconciler.Reconcile(context.Background(), repositoryRoot, RemoteSnapshot{Revision: conflictingRemote})
	diagnostics := reconciler.Diagnostics()
	if err != nil || !prepared.HasChanges || len(diagnostics.Conflicts) != 1 ||
		diagnostics.Conflicts[0].Path != "config.txt" || diagnostics.Conflicts[0].Reason != "merge_conflict" {
		t.Fatalf("isolated reconcile = %#v, diagnostics = %#v, %v", prepared, diagnostics, err)
	}
	comparisonRequest := ConflictComparisonRequest{AssignmentID: descriptor.AssignmentID, AssignmentVersion: descriptor.AssignmentVersion, Path: "config.txt", ConflictRevision: diagnostics.Conflicts[0].Revision, ExpectedRemoteRevision: conflictingRemote}
	comparison, compareErr := reconciler.CompareConflict(context.Background(), repositoryRoot, conflictingRemote, comparisonRequest)
	if compareErr != nil || string(comparison.Local.Content) != "LOCAL\ntwo\nTHREE\n" || string(comparison.Managed.Content) != "REMOTE\ntwo\nTHREE\n" || !comparison.Local.Present || !comparison.Managed.Present {
		t.Fatalf("live conflict comparison failed: %v", compareErr)
	}
	comparisonStatus := Status{State: "conflict", Mode: descriptor.Mode, RepositoryID: descriptor.RepositoryID, AssignmentID: descriptor.AssignmentID, EnvironmentID: descriptor.EnvironmentID, MachineID: descriptor.MachineID, InstallationGeneration: descriptor.InstallationGeneration, RemoteRevision: conflictingRemote, ManifestRevision: reconciler.manifest.Revision, ManifestHealth: "healthy", UpdatedAt: time.Now().UTC(), Conflicts: diagnostics.Conflicts}
	if err := WriteStatus(filepath.Join(assignmentRoot, "status.json"), comparisonStatus, descriptor.Policy.SummaryLimit); err != nil {
		t.Fatal(err)
	}
	freshReader, err := newTestFileReconciler(t, WorkspaceReconcilerConfig{HomeRoot: homeRoot, StateRoot: stateRoot, ComparisonStatusPath: filepath.Join(assignmentRoot, "status.json"), Descriptor: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	recorded, err := freshReader.CompareConflict(context.Background(), repositoryRoot, conflictingRemote, comparisonRequest)
	if err != nil || string(recorded.Local.Content) != string(comparison.Local.Content) || string(recorded.Managed.Content) != string(comparison.Managed.Content) || freshReader.mapping != nil {
		t.Fatalf("separate-process read-only comparison failed: %v", err)
	}
	// The pull-specific state owns baseline/conflict metadata, while the engine
	// publishes the assignment status one directory above it. A decoy child
	// status must neither replace that authority nor rescue a stale parent.
	decoy := comparisonStatus
	decoy.AssignmentID = "other-assignment"
	if err := WriteStatus(filepath.Join(stateRoot, "status.json"), decoy, descriptor.Policy.SummaryLimit); err != nil {
		t.Fatal(err)
	}
	if _, err := freshReader.CompareConflict(context.Background(), repositoryRoot, conflictingRemote, comparisonRequest); err != nil {
		t.Fatalf("comparison ignored explicit assignment status: %v", err)
	}
	if err := WriteStatus(filepath.Join(assignmentRoot, "status.json"), decoy, descriptor.Policy.SummaryLimit); err != nil {
		t.Fatal(err)
	}
	if _, err := freshReader.CompareConflict(context.Background(), repositoryRoot, conflictingRemote, comparisonRequest); !errors.Is(err, ErrConflictComparisonStale) {
		t.Fatalf("foreign assignment status comparison = %v", err)
	}
	if err := WriteStatus(filepath.Join(assignmentRoot, "status.json"), comparisonStatus, descriptor.Policy.SummaryLimit); err != nil {
		t.Fatal(err)
	}
	foreignRequest := comparisonRequest
	foreignRequest.AssignmentID = "other-assignment"
	if _, err := reconciler.CompareConflict(context.Background(), repositoryRoot, conflictingRemote, foreignRequest); err == nil {
		t.Fatal("foreign assignment compared")
	}
	foreignRequest = comparisonRequest
	foreignRequest.Path = "../secret"
	if _, err := reconciler.CompareConflict(context.Background(), repositoryRoot, conflictingRemote, foreignRequest); err == nil {
		t.Fatal("unsafe path compared")
	}
	foreignRequest = comparisonRequest
	foreignRequest.ExpectedRemoteRevision = baseRevision
	if _, err := reconciler.CompareConflict(context.Background(), repositoryRoot, conflictingRemote, foreignRequest); err == nil {
		t.Fatal("stale remote revision compared")
	}
	writeTestFile(t, filepath.Join(configRoot, "config.txt"), "new unsynchronized value\n")
	if _, err := reconciler.CompareConflict(context.Background(), repositoryRoot, conflictingRemote, comparisonRequest); err == nil {
		t.Fatal("edited local conflict compared as current")
	}
	writeTestFile(t, filepath.Join(configRoot, "config.txt"), "LOCAL\ntwo\nTHREE\n")
	baseVariant := filepath.Join(stateRoot, "conflicts", diagnostics.Conflicts[0].Revision, "config.txt.base")
	if value, readErr := os.ReadFile(baseVariant); readErr != nil || string(value) != "ONE\ntwo\nTHREE\n" {
		t.Fatalf("preserved base = %q", value)
	}
	for side, expected := range map[string]string{"local": "LOCAL\ntwo\nTHREE\n", "remote": "REMOTE\ntwo\nTHREE\n"} {
		path := filepath.Join(stateRoot, "conflicts", diagnostics.Conflicts[0].Revision, "config.txt."+side)
		value, readErr := os.ReadFile(path)
		info, statErr := os.Stat(path)
		if readErr != nil || statErr != nil || string(value) != expected || !privateControlFile(path, info) {
			t.Fatalf("exact private %s conflict side not preserved", side)
		}
	}
	conflictedValue, err := os.ReadFile(filepath.Join(configRoot, "config.txt"))
	if err != nil || string(conflictedValue) != "LOCAL\ntwo\nTHREE\n" {
		t.Fatalf("conflicted target changed = %q, %v", conflictedValue, err)
	}
	repositoryClean, err := os.ReadFile(filepath.Join(repositoryRoot, "clean.txt"))
	if err != nil || string(repositoryClean) != "clean local\n" {
		t.Fatalf("clean path was not prepared = %q, %v", repositoryClean, err)
	}
	if err := reconciler.PublicationCommitted(context.Background(), prepared, prepared.CommitID); err != nil {
		t.Fatal(err)
	}
	baseline, err = ReadBaseline(filepath.Join(stateRoot, "baseline.json"))
	if err != nil || baseline.Files["config.txt"] != mergedBaseState || baseline.Files["clean.txt"].Hash == "" ||
		baseline.FrozenPaths["config.txt"].ConflictRevision != diagnostics.Conflicts[0].Revision {
		t.Fatalf("isolated baseline = %#v, %v", baseline, err)
	}

	authority := &testResolutionAuthority{items: []ConflictResolution{{
		ID: "force-resolution", Path: "config.txt", Scope: "path", Action: "force_pull",
		ConflictRevision:       diagnostics.Conflicts[0].Revision,
		ExpectedRemoteRevision: prepared.CommitID,
	}}}
	reconciler.resolutions = authority
	forced, err := reconciler.Reconcile(context.Background(), repositoryRoot, RemoteSnapshot{Revision: prepared.CommitID})
	if err != nil || forced.HasChanges || len(reconciler.Diagnostics().Conflicts) != 0 {
		t.Fatalf("force pull = %#v, diagnostics = %#v, %v", forced, reconciler.Diagnostics(), err)
	}
	if err := reconciler.PublicationCommitted(context.Background(), forced, forced.CommitID); err != nil {
		t.Fatal(err)
	}
	forcedValue, err := os.ReadFile(filepath.Join(configRoot, "config.txt"))
	if err != nil || string(forcedValue) != "REMOTE\ntwo\nTHREE\n" || len(authority.acknowledged) != 1 {
		t.Fatalf("forced target = %q, acknowledgements = %#v, %v", forcedValue, authority.acknowledged, err)
	}
	baseline, err = ReadBaseline(filepath.Join(stateRoot, "baseline.json"))
	if err != nil || len(baseline.FrozenPaths) != 0 {
		t.Fatalf("force baseline = %#v, %v", baseline, err)
	}
}

func resolvedTempDir(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func testRuntimeDescriptor() RuntimeDescriptor {
	return RuntimeDescriptor{
		WriteMode: "leased_writes", Mode: ModeBidirectional,
		RepositoryID: "repository", AssignmentID: "assignment", AssignmentVersion: 1, EnvironmentID: "environment",
		MachineID: "helper", InstallationGeneration: 1, WarningRevision: "warning",
		Policy: RuntimePolicy{
			Format: "paperboat-config-plaintext-v1", Revision: "policy",
			ManifestContract: ManifestContractVersion, ManifestMaxBytes: DefaultManifestMaxBytes,
			ManifestMaxLines: DefaultManifestMaxLines, ManifestMaxPatternBytes: DefaultManifestMaxPatternBytes,
			MaxFileBytes: 1 << 20, MaxBatchBytes: 2 << 20, Debounce: time.Second,
			MinimumPushInterval: time.Minute, MaximumDirtyDelay: time.Minute,
			RemotePollInterval: time.Hour, RetryLimit: 1, ShutdownFlushTimeout: time.Second, SummaryLimit: 10,
		},
	}
}

func commitAll(t *testing.T, repository *git.Repository, message string) string {
	t.Helper()
	worktree, err := repository.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := worktree.AddWithOptions(&git.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	hash, err := worktree.Commit(message, &git.CommitOptions{Author: &object.Signature{
		Name: "Test", Email: "test@example.invalid", When: time.Unix(1, 0).UTC(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return hash.String()
}

func writeTestFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newTestFileReconciler(t *testing.T, config WorkspaceReconcilerConfig) (*PlaintextWorkspaceReconciler, error) {
	t.Helper()
	source := SourceConfig{Mode: &config.Descriptor.Mode, AutomaticUpdates: &config.Descriptor.AutomaticUpdates, Paths: map[string]RuleConfig{}}
	for i := range config.Descriptor.PathRules {
		rule := &config.Descriptor.PathRules[i]
		rule.Source = "machine"
		local, kind := rule.LocalPath, rule.Kind
		includes := append([]string{}, rule.Include...)
		excludes := append([]string{}, rule.Exclude...)
		source.Paths[rule.RepositoryPath] = RuleConfig{LocalPath: &local, Kind: &kind, Include: &includes, Exclude: &excludes}
	}
	data, err := marshalTestSource(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(resolvedTempDir(t), "config-sync.toml")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	config.MachineSource, err = LoadSourceConfig(path, DefaultSourceConfigLimits())
	if err != nil {
		t.Fatal(err)
	}
	effective, err := MergeSourceConfigs(SourceConfig{}, config.MachineSource)
	if err != nil {
		t.Fatal(err)
	}
	config.Descriptor.ConfigurationRevision = effective.Revision
	return NewPlaintextWorkspaceReconciler(config)
}

// reconcilerFixture is a repository, a home and a reconciler that has committed its first baseline.
type reconcilerFixture struct {
	repository     *git.Repository
	repositoryRoot string
	configRoot     string
	homeRoot       string
	stateRoot      string
	descriptor     RuntimeDescriptor
	reconciler     *PlaintextWorkspaceReconciler
	base           string
}

func newReconcilerFixture(t *testing.T) reconcilerFixture {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
	root := resolvedTempDir(t)
	f := reconcilerFixture{
		repositoryRoot: filepath.Join(root, "repository"), configRoot: filepath.Join(root, "selected-config"),
		homeRoot: filepath.Join(root, "home"), stateRoot: filepath.Join(root, "state", "pull"),
	}
	for _, path := range []string{f.repositoryRoot, f.homeRoot, f.stateRoot, f.configRoot} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{f.repositoryRoot, f.configRoot} {
		writeTestFile(t, filepath.Join(dir, "config.txt"), "one\ntwo\nthree\n")
	}
	var err error
	if f.repository, err = git.PlainInit(f.repositoryRoot, false); err != nil {
		t.Fatal(err)
	}
	f.base = commitAll(t, f.repository, "base")
	f.descriptor = testRuntimeDescriptor()
	f.descriptor.PathRules = []PathRule{{ID: "config", RepositoryPath: "config.txt", LocalPath: filepath.Join(f.configRoot, "config.txt"), Kind: "file"}}
	f.reconciler = f.newReconciler(t, f.descriptor)
	prepared, err := f.reconciler.Reconcile(context.Background(), f.repositoryRoot, RemoteSnapshot{Revision: f.base})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.reconciler.PublicationCommitted(context.Background(), prepared, f.base); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f reconcilerFixture) newReconciler(t *testing.T, descriptor RuntimeDescriptor) *PlaintextWorkspaceReconciler {
	t.Helper()
	reconciler, err := newTestFileReconciler(t, WorkspaceReconcilerConfig{HomeRoot: f.homeRoot, StateRoot: f.stateRoot, Descriptor: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	return reconciler
}

// A policy revision change must not strand the machine: the last synced state is still the ancestor,
// so a repository-only edit pulls cleanly instead of failing as an invalid baseline or a conflict.
func TestReconcilerPullsRepositoryEditAfterPolicyRevisionChange(t *testing.T) {
	f := newReconcilerFixture(t)
	writeTestFile(t, filepath.Join(f.repositoryRoot, "config.txt"), "one\ntwo\nTHREE\n")
	edit := commitAll(t, f.repository, "repository edit")
	changed := f.descriptor
	changed.Policy.Revision = "policy-2"
	reconciler := f.newReconciler(t, changed)
	prepared, err := reconciler.Reconcile(context.Background(), f.repositoryRoot, RemoteSnapshot{Revision: edit})
	if err != nil {
		t.Fatalf("a policy revision change failed the reconcile: %v", err)
	}
	if conflicts := reconciler.Diagnostics().Conflicts; len(conflicts) != 0 {
		t.Fatalf("repository-only edit conflicted after a policy change: %#v", conflicts)
	}
	if value, err := os.ReadFile(filepath.Join(f.configRoot, "config.txt")); err != nil || string(value) != "one\ntwo\nTHREE\n" {
		t.Fatalf("repository edit was not pulled: %q %v", value, err)
	}
	if err := reconciler.PublicationCommitted(context.Background(), prepared, edit); err != nil && !errors.Is(err, ErrBaselineInvalid) {
		t.Fatal(err)
	}
	baseline, err := ReadBaseline(filepath.Join(f.stateRoot, "baseline.json"))
	if err != nil || baseline.PolicyRevision != "policy-2" {
		t.Fatalf("baseline was not rebuilt for the new policy: %#v %v", baseline, err)
	}
}

// A baseline recorded for another repository or assignment says nothing about this one: it is
// dropped (a first reconciliation, where differing files conflict) instead of failing for good.
func TestReconcilerDropsBaselineOfAnotherRepository(t *testing.T) {
	f := newReconcilerFixture(t)
	writeTestFile(t, filepath.Join(f.repositoryRoot, "config.txt"), "one\ntwo\nTHREE\n")
	edit := commitAll(t, f.repository, "repository edit")
	other := f.descriptor
	other.RepositoryID = "another-repository"
	reconciler := f.newReconciler(t, other)
	if _, err := reconciler.Reconcile(context.Background(), f.repositoryRoot, RemoteSnapshot{Revision: edit}); err != nil {
		t.Fatalf("a foreign baseline failed the reconcile: %v", err)
	}
	conflicts := reconciler.Diagnostics().Conflicts
	if len(conflicts) != 1 || conflicts[0].Path != "config.txt" {
		t.Fatalf("without a shared ancestor the differing file must conflict, not be deleted or overwritten: %#v", conflicts)
	}
	if value, err := os.ReadFile(filepath.Join(f.configRoot, "config.txt")); err != nil || string(value) != "one\ntwo\nthree\n" {
		t.Fatalf("local file changed without an ancestor: %q %v", value, err)
	}
}

// A bidirectional assignment runs a pull and a push reconciler. The pull side's conflict was
// dropped, so the machine reported "healthy" while its file silently stopped syncing.
func TestBidirectionalDiagnosticsKeepAConflictFoundWhilePulling(t *testing.T) {
	pull := ReconciliationDiagnostics{
		Conflicts:             []PathSummary{{Path: "ecfg", Reason: "concurrent_update"}},
		Skipped:               []PathSummary{{Path: "skipped-pull"}},
		PendingCleanPathCount: 2, LastAppliedRevision: "applied",
	}
	push := ReconciliationDiagnostics{
		Conflicts: []PathSummary{{Path: "ecfg", Reason: "concurrent_update"}},
		Skipped:   []PathSummary{{Path: "skipped-push"}}, PendingCleanPathCount: 1, LastPublishedRevision: "published",
	}
	combined := CombineDirectionalDiagnostics(pull, push)
	if len(combined.Conflicts) != 1 || combined.Conflicts[0].Path != "ecfg" {
		t.Fatalf("conflicts = %+v, want the one conflict both reconcilers report, listed once", combined.Conflicts)
	}
	if combined.PendingCleanPathCount != 3 || combined.LastAppliedRevision != "applied" || combined.LastPublishedRevision != "published" || len(combined.Skipped) != 2 {
		t.Fatalf("combined = %+v", combined)
	}
}

func TestOnlyTheMatchingHalfOfASplitAssignmentCarriesOutAResolution(t *testing.T) {
	split := func(mode AssignmentMode) *PlaintextWorkspaceReconciler {
		return &PlaintextWorkspaceReconciler{descriptor: RuntimeDescriptor{Mode: mode, PullRepositoryID: "repository", PushRepositoryID: "repository"}}
	}
	for _, test := range []struct {
		mode        AssignmentMode
		action      string
		leavesToOth bool
	}{
		{ModePullOnly, "keep_local", true}, {ModePullOnly, "force_push", true}, {ModePullOnly, "keep_remote", false}, {ModePullOnly, "force_pull", false},
		{ModePushOnly, "keep_remote", true}, {ModePushOnly, "force_pull", true}, {ModePushOnly, "keep_local", false}, {ModePushOnly, "force_push", false},
	} {
		if got := split(test.mode).leavesToOtherDirection(ConflictResolution{Action: test.action}); got != test.leavesToOth {
			t.Fatalf("%s engine, %s: leaves to the other half = %v, want %v", test.mode, test.action, got, test.leavesToOth)
		}
	}
	// A plain pull-only assignment has no other half: it carries out keep_local itself.
	alone := &PlaintextWorkspaceReconciler{descriptor: RuntimeDescriptor{Mode: ModePullOnly, PullRepositoryID: "repository"}}
	if alone.leavesToOtherDirection(ConflictResolution{Action: "keep_local"}) {
		t.Fatal("a standalone pull-only assignment must handle keep_local")
	}
}
