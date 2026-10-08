package configsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
	git "github.com/go-git/go-git/v5"
)

func TestPathRulesExplicitDestinationsAndGlobs(t *testing.T) {
	home := resolvedTempDir(t)
	external := resolvedTempDir(t)
	t.Setenv("XDG_CONFIG_HOME", external)
	rules := []PathRule{{ID: "editor", RepositoryPath: "editor", LocalPath: "$XDG_CONFIG_HOME/editor", Kind: "directory", Include: []string{"**/*.json"}, Exclude: []string{"private/**"}}, {ID: "rename", RepositoryPath: "shell/config", LocalPath: "~/renamed", Kind: "file"}}
	m, err := ResolvePathRules(home, rules)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, want string
		ok         bool
	}{{"editor/settings.json", filepath.Join(external, "editor/settings.json"), true}, {"editor/sub/settings.json", filepath.Join(external, "editor/sub/settings.json"), true}, {"editor/private/key.json", "", false}, {"editor/readme.txt", "", false}, {"shell/config", filepath.Join(home, "renamed"), true}} {
		got, ok := m.LocalPath(tc.name)
		if got != tc.want || ok != tc.ok {
			t.Fatalf("%s => %s,%v", tc.name, got, ok)
		}
	}
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := ResolvePathRules(home, rules); !errors.Is(err, ErrPathRuleInvalid) {
		t.Fatalf("missing explicit token context: %v", err)
	}
}
func TestPathRulesRejectEscapesCollisionsAndLinks(t *testing.T) {
	home := resolvedTempDir(t)
	for _, rules := range [][]PathRule{
		{{ID: "a", RepositoryPath: "../escape", LocalPath: filepath.Join(home, "a"), Kind: "file"}},
		{{ID: "a", RepositoryPath: "CON.txt", LocalPath: filepath.Join(home, "a"), Kind: "file"}},
		{{ID: "a", RepositoryPath: "a:stream", LocalPath: filepath.Join(home, "a"), Kind: "file"}},
		{{ID: "a", RepositoryPath: "a", LocalPath: "relative", Kind: "file"}},
		{{ID: "a", RepositoryPath: "a", LocalPath: "%UNKNOWN%/a", Kind: "file"}},
		{{ID: "a", RepositoryPath: "a", LocalPath: filepath.Join(home, "a"), Kind: "file", Include: []string{"*"}}},
		{{ID: "a", RepositoryPath: "dir", LocalPath: filepath.Join(home, "a"), Kind: "directory"}, {ID: "b", RepositoryPath: "dir/file", LocalPath: filepath.Join(home, "b"), Kind: "file"}},
		{{ID: "a", RepositoryPath: "a", LocalPath: filepath.Join(home, "dest"), Kind: "file"}, {ID: "b", RepositoryPath: "b", LocalPath: filepath.Join(home, "DEST"), Kind: "file"}},
	} {
		if _, err := ResolvePathRules(home, rules); !errors.Is(err, ErrPathRuleInvalid) {
			t.Fatalf("accepted %#v: %v", rules, err)
		}
	}
	link := filepath.Join(home, "link")
	if err := os.Symlink(resolvedTempDir(t), link); err != nil {
		t.Skip(err)
	}
	if _, err := ResolvePathRules(home, []PathRule{{ID: "a", RepositoryPath: "a", LocalPath: filepath.Join(link, "file"), Kind: "file"}}); !errors.Is(err, ErrPathRuleInvalid) {
		t.Fatalf("symlink: %v", err)
	}
}
func TestMappedSnapshotExclusionsApplyPhysicalAndLogicalPaths(t *testing.T) {
	home := resolvedTempDir(t)
	repository := resolvedTempDir(t)
	writeTestFile(t, filepath.Join(home, ".env"), "never")
	writeTestFile(t, filepath.Join(repository, "innocent"), "never")
	manifest, err := ParseManifest([]byte("innocent\n"), nil, DefaultManifestLimits())
	if err != nil {
		t.Fatal(err)
	}
	m, err := ResolvePathRules(home, []PathRule{{ID: "rename", RepositoryPath: "innocent", LocalPath: filepath.Join(home, ".env"), Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, local := range []bool{true, false} {
		snapshot, err := m.Snapshot(testRuntimeDescriptor().Policy, manifest, repository, local)
		if err != nil || len(snapshot.Files) != 0 {
			t.Fatalf("physical exclusion local=%v: %#v %v", local, snapshot, err)
		}
	}
}
func TestMappedJournalRecoversOriginalMappingAfterReconfiguration(t *testing.T) {
	home := resolvedTempDir(t)
	oldDest := filepath.Join(resolvedTempDir(t), "old")
	newDest := filepath.Join(resolvedTempDir(t), "new")
	writeTestFile(t, oldDest, "before")
	old, err := ResolvePathRules(home, []PathRule{{ID: "rule", RepositoryPath: "config", LocalPath: oldDest, Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := ResolvePathRules(home, []PathRule{{ID: "rule", RepositoryPath: "config", LocalPath: newDest, Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(resolvedTempDir(t), "apply.json")
	if err := beginApplyJournal(journal, home, "repo", "assignment", "remote", []string{"config"}, 1024, old); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, oldDest, "interrupted")
	if err := recoverApplyJournal(journal, home, "repo", "assignment", 1024, current); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(oldDest)
	if string(got) != "before" {
		t.Fatalf("old destination: %q", got)
	}
	if _, err := os.Stat(newDest); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new destination touched: %v", err)
	}
	if _, err := os.Stat(journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal not cleaned: %v", err)
	}
}

func TestMappedReconcileRetainsUnselectedRepositoryAndRelinquishesPaths(t *testing.T) {
	home := resolvedTempDir(t)
	state := resolvedTempDir(t)
	repoRoot := resolvedTempDir(t)
	external := resolvedTempDir(t)
	if err := os.Mkdir(filepath.Join(repoRoot, "editor"), 0700); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(repoRoot, "editor/config.json"), "old")
	writeTestFile(t, filepath.Join(repoRoot, "editor/unselected.txt"), "retain subset")
	writeTestFile(t, filepath.Join(repoRoot, "untouched"), "retain unrelated")
	writeTestFile(t, filepath.Join(external, "config.json"), "old")
	repo, err := git.PlainInit(repoRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	revision := commitAll(t, repo, "initial")
	descriptor := testRuntimeDescriptor()
	descriptor.PathRules = []PathRule{{ID: "editor", RepositoryPath: "editor", LocalPath: external, Kind: "directory", Include: []string{"*.json"}}}
	reconcile := func(desc RuntimeDescriptor, head string) (*PlaintextWorkspaceReconciler, string) {
		t.Helper()
		r, err := newTestFileReconciler(t, WorkspaceReconcilerConfig{HomeRoot: home, StateRoot: state, Descriptor: desc})
		if err != nil {
			t.Fatal(err)
		}
		p, err := r.Reconcile(context.Background(), repoRoot, RemoteSnapshot{Revision: head})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.PublicationCommitted(context.Background(), p, p.CommitID); err != nil {
			t.Fatal(err)
		}
		return r, p.CommitID
	}
	_, revision = reconcile(descriptor, revision)
	writeTestFile(t, filepath.Join(external, "config.json"), "new local")
	_, revision = reconcile(descriptor, revision)
	for name, want := range map[string]string{"editor/config.json": "new local", "editor/unselected.txt": "retain subset", "untouched": "retain unrelated"} {
		got, _ := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(name)))
		if string(got) != want {
			t.Fatalf("%s: %q", name, got)
		}
	}
	descriptor.PathRules = nil
	_, revision = reconcile(descriptor, revision)
	got, _ := os.ReadFile(filepath.Join(external, "config.json"))
	if string(got) != "new local" {
		t.Fatalf("removed rule deleted local: %q", got)
	}
	got, _ = os.ReadFile(filepath.Join(repoRoot, "editor/config.json"))
	if string(got) != "new local" {
		t.Fatalf("removed rule deleted repo: %q", got)
	}
	baseline, err := ReadBaseline(filepath.Join(state, "baseline.json"))
	if err != nil || len(baseline.Files) != 0 {
		t.Fatalf("relinquished baseline: %#v %v", baseline, err)
	}
}

func TestMappedPullDeletionOwnsOnlySelectedFiles(t *testing.T) {
	home := resolvedTempDir(t)
	state := resolvedTempDir(t)
	repoRoot := resolvedTempDir(t)
	external := resolvedTempDir(t)
	writeTestFile(t, filepath.Join(repoRoot, "config"), "remote")
	writeTestFile(t, filepath.Join(external, "unrelated"), "keep")
	repo, err := git.PlainInit(repoRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	head := commitAll(t, repo, "initial")
	descriptor := testRuntimeDescriptor()
	descriptor.Mode = ModePullOnly
	descriptor.PathRules = []PathRule{{ID: "mapped", RepositoryPath: "config", LocalPath: filepath.Join(external, "renamed"), Kind: "file"}}
	r, err := newTestFileReconciler(t, WorkspaceReconcilerConfig{HomeRoot: home, StateRoot: state, Descriptor: descriptor})
	if err != nil {
		t.Fatal(err)
	}
	reconcile := func() {
		t.Helper()
		p, err := r.Reconcile(context.Background(), repoRoot, RemoteSnapshot{Revision: head})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.PublicationCommitted(context.Background(), p, p.CommitID); err != nil {
			t.Fatal(err)
		}
	}
	reconcile()
	engine, err := NewEngine(EngineConfig{HomeRoot: home, Descriptor: descriptor, Syncer: failingSyncer{}, Manifest: r})
	if err != nil {
		t.Fatal(err)
	}
	if engine.managedEvent(filepath.Join(external, "renamed")) {
		t.Fatal("own pull output retriggered watcher")
	}
	got, _ := os.ReadFile(filepath.Join(external, "renamed"))
	if string(got) != "remote" {
		t.Fatalf("pull rename: %q", got)
	}
	if err := os.Remove(filepath.Join(repoRoot, "config")); err != nil {
		t.Fatal(err)
	}
	head = commitAll(t, repo, "upstream deletion")
	reconcile()
	if _, err := os.Stat(filepath.Join(external, "renamed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("upstream deletion unapplied: %v", err)
	}
	if engine.managedEvent(filepath.Join(external, "renamed")) {
		t.Fatal("own upstream deletion retriggered watcher")
	}
	writeTestFile(t, filepath.Join(external, "renamed"), "new user content")
	if !engine.managedEvent(filepath.Join(external, "renamed")) {
		t.Fatal("user edit suppressed after own deletion")
	}
	if engine.managedEvent(filepath.Join(external, ".paperboat-config-staging")) {
		t.Fatal("atomic staging feeds watcher")
	}
	got, _ = os.ReadFile(filepath.Join(external, "unrelated"))
	if string(got) != "keep" {
		t.Fatalf("unrelated file touched: %q", got)
	}
}

func TestMappedApplyRefusesUnwritableDestinationAndPreservesPreimage(t *testing.T) {
	home := resolvedTempDir(t)
	target := filepath.Join(home, "config")
	writeTestFile(t, target, "before")
	if err := os.Chmod(target, 0400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(target, 0600)
	m, err := ResolvePathRules(home, []PathRule{{ID: "mapped", RepositoryPath: "config", LocalPath: target, Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.write("config", []byte("after"), 0600); !errors.Is(err, ErrPathRuleInvalid) {
		t.Fatalf("unwritable write: %v", err)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "before" {
		t.Fatalf("preimage changed: %q", got)
	}
}

func TestMappingRuntimeStateExclusionCannotBeRenamedAway(t *testing.T) {
	home := resolvedTempDir(t)
	runtimeRoot := resolvedTempDir(t)
	repository := resolvedTempDir(t)
	target := filepath.Join(runtimeRoot, "repository-credentials")
	writeTestFile(t, target, "private state")
	m, err := ResolvePathRules(home, []PathRule{{ID: "rename", RepositoryPath: "innocent", LocalPath: target, Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseManifest([]byte("innocent\n"), nil, DefaultManifestLimits())
	if err != nil {
		t.Fatal(err)
	}
	policy := testRuntimeDescriptor().Policy
	policy.AbsoluteRuntimeExclusionRoots = []string{runtimeRoot}
	snapshot, err := m.Snapshot(policy, manifest, repository, true)
	if err != nil || len(snapshot.Files) != 0 {
		t.Fatalf("runtime state exposed: %#v %v", snapshot, err)
	}
	if m.managedEvent(target, policy, manifest) {
		t.Fatal("runtime output feeds watcher")
	}
}

func TestMappingEditRetainsUnchangedPathMergeAncestry(t *testing.T) {
	home := resolvedTempDir(t)
	state := resolvedTempDir(t)
	repository := resolvedTempDir(t)
	external := resolvedTempDir(t)
	for _, name := range []string{"a", "b"} {
		writeTestFile(t, filepath.Join(repository, name), "one\ntwo\nthree\n")
		writeTestFile(t, filepath.Join(external, name), "one\ntwo\nthree\n")
	}
	repo, err := git.PlainInit(repository, false)
	if err != nil {
		t.Fatal(err)
	}
	head := commitAll(t, repo, "initial")
	descriptor := testRuntimeDescriptor()
	descriptor.PathRules = []PathRule{{ID: "a", RepositoryPath: "a", LocalPath: filepath.Join(external, "a"), Kind: "file"}, {ID: "b", RepositoryPath: "b", LocalPath: filepath.Join(external, "b"), Kind: "file"}}
	reconcile := func() {
		t.Helper()
		r, err := newTestFileReconciler(t, WorkspaceReconcilerConfig{HomeRoot: home, StateRoot: state, Descriptor: descriptor})
		if err != nil {
			t.Fatal(err)
		}
		p, err := r.Reconcile(context.Background(), repository, RemoteSnapshot{Revision: head})
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Diagnostics().Conflicts) != 0 {
			t.Fatalf("unchanged mapping lost ancestor: %#v", r.Diagnostics())
		}
		if err := r.PublicationCommitted(context.Background(), p, p.CommitID); err != nil {
			t.Fatal(err)
		}
		head = p.CommitID
	}
	reconcile()
	descriptor.PathRules = descriptor.PathRules[:1]
	writeTestFile(t, filepath.Join(external, "a"), "ONE\ntwo\nthree\n")
	writeTestFile(t, filepath.Join(repository, "a"), "one\ntwo\nTHREE\n")
	head = commitAll(t, repo, "remote edit")
	reconcile()
	got, _ := os.ReadFile(filepath.Join(external, "a"))
	if string(got) != "ONE\ntwo\nTHREE\n" {
		t.Fatalf("merge ancestry lost: %q", got)
	}
	got, _ = os.ReadFile(filepath.Join(external, "b"))
	if string(got) != "one\ntwo\nthree\n" {
		t.Fatalf("relinquished path changed: %q", got)
	}
}

func TestExplicitWindowsKnownLocationTokensUseSuppliedUserContext(t *testing.T) {
	home := resolvedTempDir(t)
	target := resolvedTempDir(t)
	for _, key := range []string{"APPDATA", "LOCALAPPDATA", "USERPROFILE"} {
		t.Setenv(key, target)
		m, err := ResolvePathRules(home, []PathRule{{ID: "rule", RepositoryPath: "settings", LocalPath: "%" + key + "%/settings", Kind: "file"}})
		if err != nil {
			t.Fatalf("%s: %v", key, err)
		}
		got, ok := m.LocalPath("settings")
		if !ok || got != filepath.Join(target, "settings") {
			t.Fatalf("%s expansion: %s", key, got)
		}
		t.Setenv(key, "")
		if _, err := ResolvePathRules(home, []PathRule{{ID: "rule", RepositoryPath: "settings", LocalPath: "%" + key + "%/settings", Kind: "file"}}); !errors.Is(err, ErrPathRuleInvalid) {
			t.Fatalf("%s silently guessed absent context: %v", key, err)
		}
	}
}

func TestMappedSnapshotBoundsZeroByteEntriesAndHonorsCancellation(t *testing.T) {
	home := resolvedTempDir(t)
	directory := resolvedTempDir(t)
	for _, name := range []string{"a", "b", "c"} {
		writeTestFile(t, filepath.Join(directory, name), "")
	}
	m, err := ResolvePathRules(home, []PathRule{{ID: "directory", RepositoryPath: "configs", LocalPath: directory, Kind: "directory"}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseManifest([]byte("configs/\n"), nil, DefaultManifestLimits())
	if err != nil {
		t.Fatal(err)
	}
	policy := testRuntimeDescriptor().Policy
	policy.ManifestMaxLines = 2
	if _, err := m.Snapshot(policy, manifest, home, true); !errors.Is(err, ErrSnapshotInvalid) {
		t.Fatalf("unbounded zero-byte entries: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.SnapshotContext(ctx, policy, manifest, home, true); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}

func TestExplicitExpansionRejectsTraversalBeforeCleaning(t *testing.T) {
	home := resolvedTempDir(t)
	t.Setenv("XDG_CONFIG_HOME", home)
	for _, path := range []string{"~/../escape", "~/./file", "$XDG_CONFIG_HOME/../escape"} {
		if _, err := ResolvePathRules(home, []PathRule{{ID: "rule", RepositoryPath: "config", LocalPath: path, Kind: "file"}}); !errors.Is(err, ErrPathRuleInvalid) {
			t.Fatalf("expanded unsafe destination %s: %v", path, err)
		}
	}
}

func TestMappedWatchScopeExcludesRuntimeAndUserExcludedDirectories(t *testing.T) {
	home := resolvedTempDir(t)
	selected := resolvedTempDir(t)
	runtimeRoot := filepath.Join(selected, "runtime")
	for _, path := range []string{runtimeRoot, filepath.Join(selected, "private"), filepath.Join(selected, "sub")} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	m, err := ResolvePathRules(home, []PathRule{{ID: "dir", RepositoryPath: "configs", LocalPath: selected, Kind: "directory", Exclude: []string{"private/**"}}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseManifest([]byte("configs/\n"), nil, DefaultManifestLimits())
	if err != nil {
		t.Fatal(err)
	}
	policy := testRuntimeDescriptor().Policy
	policy.AbsoluteRuntimeExclusionRoots = []string{runtimeRoot}
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if err := resetMappedWatches(watcher, m, policy, manifest); err != nil {
		t.Fatal(err)
	}
	watched := map[string]bool{}
	for _, path := range watcher.WatchList() {
		watched[path] = true
	}
	if watched[home] || watched[runtimeRoot] || watched[filepath.Join(selected, "private")] || !watched[selected] || !watched[filepath.Join(selected, "sub")] {
		t.Fatalf("watch scope: %#v", watched)
	}
	empty, err := ResolvePathRules(home, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := resetMappedWatches(watcher, empty, policy, manifest); err != nil {
		t.Fatal(err)
	}
	if len(watcher.WatchList()) != 0 {
		t.Fatal("empty rules left filesystem watches")
	}
}

func TestMandatoryExclusionWindowsDriveAncestorTraversalTerminates(t *testing.T) {
	for _, name := range []string{"C:/Users/example/editor/settings.json", "C:/", "C:"} {
		if mandatoryExcluded(name, RuntimePolicy{}) {
			t.Fatalf("ordinary physical name incorrectly excluded: %s", name)
		}
	}
	if !mandatoryExcluded("C:/Users/example/.ssh/key", RuntimePolicy{}) {
		t.Fatal("Windows physical secret exclusion lost")
	}
}

func TestMappedSecretExclusionCannotBeBypassedByCaseAlias(t *testing.T) {
	home := resolvedTempDir(t)
	directory := resolvedTempDir(t)
	target := filepath.Join(directory, ".ENV.production")
	writeTestFile(t, target, "never")
	m, err := ResolvePathRules(home, []PathRule{{ID: "rename", RepositoryPath: "settings", LocalPath: target, Kind: "file"}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := ParseManifest([]byte("settings\n"), nil, DefaultManifestLimits())
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(testRuntimeDescriptor().Policy, manifest, home, true)
	if err != nil || len(snapshot.Files) != 0 {
		t.Fatalf("case alias secret exposed: %#v %v", snapshot, err)
	}
}
