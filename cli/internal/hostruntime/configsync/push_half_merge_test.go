package configsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// Production runs a bidirectional assignment as a pull half and a push half; only the push half publishes, so only it can merge.
func TestPushHalfMergesAnEditMadeOnBothSides(t *testing.T) {
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
	descriptor.Mode = ModePushOnly // the push half of a split bidirectional assignment
	descriptor.PathRules = []PathRule{{ID: "config", RepositoryPath: "config.txt", LocalPath: filepath.Join(configRoot, "config.txt"), Kind: "file"}, {ID: "clean", RepositoryPath: "clean.txt", LocalPath: filepath.Join(configRoot, "clean.txt"), Kind: "file"}}
	reconciler, err := newTestFileReconciler(t, WorkspaceReconcilerConfig{
		HomeRoot: homeRoot, StateRoot: stateRoot, Descriptor: descriptor, MergeConcurrentEdits: true,
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

}
