package configsync

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
)

// The other machine already merged an edit made on both sides and published it; this pull half still holds its own
// edit against the old base, which is a conflict until the merge is seen to equal the remote file.
func TestPullHalfAppliesARemoteThatAlreadyMergedItsEdit(t *testing.T) {
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
	descriptor.Mode = ModePullOnly // the pull half of a split bidirectional assignment
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
	writeTestFile(t, filepath.Join(repositoryRoot, "config.txt"), "ONE\ntwo\nTHREE\n")
	remoteRevision := commitAll(t, repository, "remote change")
	prepared, err = reconciler.Reconcile(context.Background(), repositoryRoot, RemoteSnapshot{Revision: remoteRevision})
	if err != nil || prepared.HasChanges {
		t.Fatalf("reconcile = %#v, diagnostics = %#v, %v", prepared, reconciler.Diagnostics(), err)
	}
	value, err := os.ReadFile(filepath.Join(configRoot, "config.txt"))
	if err != nil || string(value) != "ONE\ntwo\nTHREE\n" {
		t.Fatalf("target = %q, %v", value, err)
	}
	if conflicts := reconciler.Diagnostics().Conflicts; len(conflicts) != 0 {
		t.Fatalf("conflicts = %#v", conflicts)
	}
}
